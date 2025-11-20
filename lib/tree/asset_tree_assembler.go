package tree

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/assets"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
)

// AssetTreeConfig carries the asset-level parameters needed to build and
// finalize the tree. It decorates the structural tree with asset semantics.
// Note: Tree nodes are always zero-fee since they use ephemeral anchors and
// are broadcast via package relay with an external fee-paying child.
type AssetTreeConfig struct {
	AssetID     asset.ID
	CSVDelay    uint32
	OperatorKey *btcec.PublicKey
	ChainParams *address.ChainParams
}

// AssetTreeAssembler builds a virtual asset tree top-down using AssetTxBuilder
// at each node, harvesting anchor plans/control blocks/proofs and wiring them
// into the tree nodes so existing MuSig2 signing/sweep flows work unchanged.
type AssetTreeAssembler struct {
	cfg    AssetTreeConfig
	wallet assets.AssetWalletClient
}

// NewAssetTreeAssembler constructs an assembler.
func NewAssetTreeAssembler(cfg AssetTreeConfig,
	wallet assets.AssetWalletClient) *AssetTreeAssembler {

	return &AssetTreeAssembler{
		cfg:    cfg,
		wallet: wallet,
	}
}

// BuildTree assembles a full asset-aware tree using a two-pass approach:
//
// Pass 1 (bottom-up): Build tree structure from leaf descriptors, computing
// cosigners, internal keys, and asset amounts at each level. No transactions
// or proofs are built yet.
//
// Pass 2 (top-down): Materialize transactions starting from the root, filling
// in Input, Outputs, proofs, and plans as we traverse down the tree.
//
// This separation makes the tree construction logic clearer: structure is
// determined purely by the leaf descriptors and radix, while transaction
// building follows the natural spending order (parent must exist before child
// can spend it).
func (a *AssetTreeAssembler) BuildTree(ctx context.Context,
	rootInput wire.OutPoint, rootPlan *assets.AnchorPlan, rootProof []byte,
	rootOutput *wire.TxOut, leaves []LeafDescriptor, radix int) (*Tree,
	error) {

	if len(leaves) == 0 {
		return nil, fmt.Errorf("no leaves supplied")
	}
	if rootOutput == nil {
		return nil, fmt.Errorf("root output cannot be nil")
	}

	// Pass 1: Build tree structure bottom-up. This determines the tree
	// shape, partitions leaves into groups, and computes cosigners and
	// internal keys at each level.
	structCfg := TreeStructureConfig{
		OperatorKey: a.cfg.OperatorKey,
		Radix:       radix,
		WeightFn:    WeightByAssetAmountOrBTC(),
	}

	root, err := BuildTreeStructure(leaves, structCfg)
	if err != nil {
		return nil, fmt.Errorf("build tree structure: %w", err)
	}

	// Pass 2: Materialize transactions top-down. Starting from the root,
	// build transactions at each node using the structure from pass 1.
	mat := NewAssetMaterializer(a.cfg, a.wallet)

	// Compute BTC value per child for the root node. For leaves, this is
	// unused, but for branches we need to split the root's BTC value evenly
	// among children.
	numChildren := len(root.Children)
	childBtcValue := int64(0)
	if numChildren > 0 {
		childBtcValue = rootOutput.Value / int64(numChildren)
	}

	matParams := MaterializeParams{
		Input:         rootInput,
		ParentProof:   rootProof,
		ParentPlan:    rootPlan,
		InputBtcValue: rootOutput.Value,
		ChildBtcValue: childBtcValue,
	}

	if err := Materialize(ctx, root, matParams, mat); err != nil {
		return nil, fmt.Errorf("materialize tree: %w", err)
	}

	return &Tree{
		Root:               root,
		BatchOutpoint:      rootInput,
		BatchOutput:        rootOutput,
		SweepTapscriptRoot: nil,
	}, nil
}

// firstProofSuffix extracts the first proof suffix bytes from a list of vpkts.
func firstProofSuffix(pkts []*tappsbt.VPacket) ([]byte, error) {
	if len(pkts) == 0 || len(pkts[0].Outputs) == 0 {
		return nil, fmt.Errorf("no vpacket outputs")
	}
	pf := pkts[0].Outputs[0].ProofSuffix
	if pf == nil {
		return nil, fmt.Errorf("proof suffix missing")
	}

	file, err := proof.NewFile(proof.V0, *pf)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := file.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// proofsByOutput collects proof suffix bytes per output index.
func proofsByOutput(pkts []*tappsbt.VPacket, count int) ([][]byte, error) {
	out := make([][]byte, count)
	if len(pkts) == 0 {
		return out, fmt.Errorf("no vpackets for proofs")
	}
	pkt := pkts[0]
	if len(pkt.Outputs) < count {
		return out, fmt.Errorf("vpacket outputs < count")
	}
	for i := 0; i < count; i++ {
		pf := pkt.Outputs[i].ProofSuffix
		if pf == nil {
			return nil, fmt.Errorf("proof suffix missing for "+
				"output %d", i)
		}

		file, err := proof.NewFile(proof.V0, *pf)
		if err != nil {
			return nil, err
		}

		var buf bytes.Buffer
		if err := file.Encode(&buf); err != nil {
			return nil, err
		}

		out[i] = buf.Bytes()
	}
	return out, nil
}

// attachTaprootTweakFromParent extracts the taproot asset root from the parent
// proof (the input being spent) and stores it in the node's TaprootTweak field.
// This ensures tree signing uses the same tweak that tapd applied when creating
// the parent's output (which is now this node's input). It also recomputes
// node.FinalKey using MuSig2 aggregation with the tweak. When the proof omits
// the tapscript sibling (common for script-controlled VTXOs), the sibling is
// reconstructed from the anchor plan's script plans.
func attachTaprootTweakFromParent(node *Node, parentPlan *assets.AnchorPlan,
	parentProof []byte) error {

	if node == nil || len(parentProof) == 0 {
		return nil
	}

	// Decode the parent proof to extract the taproot root.
	file, err := proof.DecodeFile(parentProof)
	if err != nil {
		return fmt.Errorf("decode parent proof: %w", err)
	}

	lastProof, err := file.LastProof()
	if err != nil {
		return fmt.Errorf("get last proof: %w", err)
	}

	// Reconstruct the tapscript sibling. Prefer the sibling encoded in the
	// proof; otherwise, rebuild it from the anchor plan script leaves when
	// present (common for script-controlled leaves).
	var siblingHash *chainhash.Hash
	if lastProof.InclusionProof.CommitmentProof != nil &&
		lastProof.InclusionProof.CommitmentProof.TapSiblingPreimage != nil {

		hash, err := lastProof.InclusionProof.CommitmentProof.
			TapSiblingPreimage.TapHash()
		if err != nil {
			return fmt.Errorf("compute sibling hash: %w", err)
		}
		siblingHash = hash
	} else if parentPlan != nil && parentPlan.Witness.ScriptPlans != nil {
		var leaves []txscript.TapLeaf
		for _, sp := range parentPlan.Witness.ScriptPlans {
			if sp == nil {
				continue
			}
			leaves = append(leaves, sp.TapLeaf)
		}
		if len(leaves) > 0 {
			tree := txscript.AssembleTaprootScriptTree(leaves...)
			root := tree.RootNode.TapHash()
			siblingHash = &root
		}
	}

	// Get the actual output key from the anchor transaction in the proof.
	// This is the definitive output key that was committed on-chain.
	outIdx := lastProof.InclusionProof.OutputIndex
	if int(outIdx) >= len(lastProof.AnchorTx.TxOut) {
		return fmt.Errorf("output index %d out of range", outIdx)
	}
	anchorOut := lastProof.AnchorTx.TxOut[outIdx]
	actualOutputKey, err := taprootKeyFromScript(anchorOut.PkScript)
	if err != nil {
		return fmt.Errorf("extract output key from anchor: %w", err)
	}

	// Use the actual output key as our FinalKey since that's what we need
	// to sign for. The MuSig2 session will be created with node.CoSigners
	// and node.TaprootTweak, and it should produce the same key.
	node.FinalKey = actualOutputKey

	// Derive the asset commitment from the proof. DeriveByAssetInclusion
	// returns a map keyed by output keys (both V2 and non-V2 versions).
	// We select the commitment that matches our actual on-chain output key.
	keys, err := lastProof.InclusionProof.DeriveByAssetInclusion(
		&lastProof.Asset, nil,
	)
	if err != nil {
		return fmt.Errorf("derive by asset inclusion: %w", err)
	}

	// Look up the commitment by the actual output key.
	actualOutputKeySer := asset.ToSerialized(actualOutputKey)
	tapCommitment, ok := keys[actualOutputKeySer]
	if !ok {
		// Fall back to the first available commitment if we can't find
		// one matching the output key exactly.
		for _, c := range keys {
			tapCommitment = c
			break
		}
	}
	if tapCommitment == nil {
		return fmt.Errorf("tap commitment missing from proof")
	}

	taproot := tapCommitment.TapscriptRoot(siblingHash)
	taprootRoot := taproot[:]
	node.TaprootTweak = taprootRoot

	return nil
}

// taprootKeyFromScript extracts the taproot output key from a standard P2TR
// script, returning an error when the script is not taproot or malformed.
func taprootKeyFromScript(pkScript []byte) (*btcec.PublicKey, error) {
	if len(pkScript) != 34 {
		return nil, fmt.Errorf("unexpected pkscript len %d", len(pkScript))
	}

	if pkScript[0] != txscript.OP_1 || pkScript[1] != txscript.OP_DATA_32 {
		return nil, fmt.Errorf("not a taproot script")
	}

	return schnorr.ParsePubKey(pkScript[2:])
}
