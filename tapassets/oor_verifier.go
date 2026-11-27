package tapassets

import (
	"bytes"
	"context"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/customanchor"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
)

// OORLineageAnchor identifies one confirmed commitment transaction and its
// extracted commitment-tree path contributing lineage to an asset VTXO.
type OORLineageAnchor struct {
	// CommitmentTxID is the txid of the confirmed commitment transaction.
	CommitmentTxID chainhash.Hash

	// TreePath is the extracted commitment-tree fragment from the batch
	// root down to the leaf VTXO served by this commitment.
	TreePath *tree.Tree
}

// OORPackageTxs holds the unsigned or finalized transactions of one OOR
// package hop (its Ark transaction and checkpoint transactions).
type OORPackageTxs struct {
	// ArkTx is the Ark transaction of this OOR hop.
	ArkTx *wire.MsgTx

	// CheckpointTxs are the checkpoint transactions spent by ArkTx.
	CheckpointTxs []*wire.MsgTx
}

// VerifyOORAssetVTXORequest carries the authenticated Bitcoin lineage, OOR
// package chain, and sealed Ark asset package for one incoming OOR asset
// output.
type VerifyOORAssetVTXORequest struct {
	// AssetRef is the canonical SDK asset reference expected in Outpoint.
	AssetRef string

	// AssetAmount is the expected total asset unit balance in Outpoint.
	AssetAmount uint64

	// AssetRoot is the expected 32-byte Taproot Asset root committed in
	// Outpoint.
	AssetRoot chainhash.Hash

	// Outpoint is the Ark transaction output being verified.
	Outpoint wire.OutPoint

	// ArkTx is the Ark transaction that creates Outpoint.
	ArkTx *wire.MsgTx

	// CheckpointTxs are the checkpoint transactions spent by ArkTx.
	CheckpointTxs []*wire.MsgTx

	// AncestorPackages are prior finalized OOR packages leading into
	// CheckpointTxs.
	AncestorPackages []OORPackageTxs

	// Lineage holds every commitment-tree fragment anchoring this VTXO.
	Lineage []OORLineageAnchor

	// SealedArkPackage is the sealed tap-sdk transition package for ArkTx.
	SealedArkPackage []byte
}

// oorProofVerifier verifies confirmed proofs via ProofChainVerifier and binds
// every unconfirmed transition step to the authenticated commitment-tree
// lineage and OOR package transactions of the VTXO.
type oorProofVerifier struct {
	chain        ProofChainVerifier
	transactions map[chainhash.Hash]struct{}
	anchors      map[tapsdk.Outpoint]struct{}
}

// NewOORProofVerifier returns a customanchor.ConfirmedProofVerifier that checks
// confirmed base proofs via chain and restricts every unconfirmed step to the
// transactions and outputs present in lineage and packages.
func NewOORProofVerifier(chain ProofChainVerifier, lineage []OORLineageAnchor,
	packages []OORPackageTxs) (customanchor.ConfirmedProofVerifier, error) {

	if chain == nil {
		return nil, fmt.Errorf("proof chain verifier is required")
	}
	if len(lineage) == 0 && len(packages) == 0 {
		return nil, fmt.Errorf("at least one lineage anchor or OOR " +
			"package is required")
	}

	v := &oorProofVerifier{
		chain:        chain,
		transactions: make(map[chainhash.Hash]struct{}),
		anchors:      make(map[tapsdk.Outpoint]struct{}),
	}

	for idx, anc := range lineage {
		if anc.CommitmentTxID == (chainhash.Hash{}) {
			return nil, fmt.Errorf("lineage %d commitment txid is "+
				"required", idx)
		}
		if anc.TreePath == nil || anc.TreePath.Root == nil {
			return nil, fmt.Errorf("lineage %d tree path is "+
				"required", idx)
		}

		v.transactions[anc.CommitmentTxID] = struct{}{}
		for node := range anc.TreePath.Root.NodesIter() {
			tx, err := node.ToTx()
			if err != nil {
				return nil, fmt.Errorf("lineage %d tree tx: %w",
					idx, err)
			}
			v.transactions[tx.TxHash()] = struct{}{}
			v.anchors[sdkOutpoint(node.Input)] = struct{}{}

			if node.IsLeaf() {
				leafOP, err := node.GetNonAnchorOutpoint()
				if err != nil {
					return nil, fmt.Errorf("lineage %d "+
						"leaf outpoint: %w", idx, err)
				}
				v.anchors[sdkOutpoint(*leafOP)] = struct{}{}
			}
		}
	}

	for idx, pkg := range packages {
		if pkg.ArkTx == nil {
			return nil, fmt.Errorf("oor package %d ark tx is "+
				"required", idx)
		}
		arkTxID := pkg.ArkTx.TxHash()
		v.transactions[arkTxID] = struct{}{}
		for outIdx, out := range pkg.ArkTx.TxOut {
			if arktx.IsAnchorOutput(out) {
				continue
			}
			v.anchors[sdkOutpoint(wire.OutPoint{
				Hash:  arkTxID,
				Index: uint32(outIdx),
			})] = struct{}{}
		}

		for cpIdx, cpTx := range pkg.CheckpointTxs {
			if cpTx == nil {
				return nil, fmt.Errorf("oor package %d "+
					"checkpoint %d is nil", idx, cpIdx)
			}
			cpTxID := cpTx.TxHash()
			v.transactions[cpTxID] = struct{}{}
			v.anchors[sdkOutpoint(wire.OutPoint{
				Hash:  cpTxID,
				Index: 0,
			})] = struct{}{}
		}
	}

	return v, nil
}

// VerifyConfirmedProof checks the confirmed deposit proof chain and derives its
// confirmed anchor inventory.
func (v *oorProofVerifier) VerifyConfirmedProof(ctx context.Context,
	proofFile []byte) (*customanchor.ConfirmedProofVerification, error) {

	if err := v.chain.VerifyProofChain(ctx, proofFile); err != nil {
		return nil, err
	}

	return customanchor.DeriveConfirmedProofInventory(proofFile)
}

// VerifyUnconfirmedAnchor ensures every unconfirmed step anchors its asset in a
// transaction and output authenticated by the VTXO's lineage or OOR package
// chain.
func (v *oorProofVerifier) VerifyUnconfirmedAnchor(_ context.Context,
	transition customanchor.UnconfirmedAnchorVerification) error {

	var tx wire.MsgTx
	err := tx.Deserialize(bytes.NewReader(transition.AnchorTransaction))
	if err != nil {
		return fmt.Errorf("decode step %d transaction: %w",
			transition.StepIndex, err)
	}

	txid := tx.TxHash()
	if _, ok := v.transactions[txid]; !ok {
		return fmt.Errorf("step %d transaction %v is not in the "+
			"VTXO lineage", transition.StepIndex, txid)
	}
	if chainhash.Hash(transition.AnchorOutpoint.Txid) != txid {
		return fmt.Errorf("step %d anchor is not in its transaction",
			transition.StepIndex)
	}
	if _, ok := v.anchors[transition.AnchorOutpoint]; !ok {
		return fmt.Errorf("step %d anchor %v is not on the VTXO path",
			transition.StepIndex, transition.AnchorOutpoint)
	}

	return nil
}

// VerifyOORAssetVTXO verifies that SealedArkPackage seals req.ArkTx and moves
// exactly req.AssetAmount units of req.AssetRef into req.Outpoint under
// OP_TRUE script keys committing to req.AssetRoot, with every piece's proof
// path (including merged co-input paths) verified back to confirmed issuance
// through req.Lineage and the OOR package chain.
func (v *VTXOVerifier) VerifyOORAssetVTXO(ctx context.Context,
	req VerifyOORAssetVTXORequest) error {

	switch {
	case v == nil || v.chain == nil:
		return fmt.Errorf("proof chain verifier is required")

	case req.ArkTx == nil:
		return fmt.Errorf("ark transaction is required")

	case req.Outpoint.Hash != req.ArkTx.TxHash():
		return fmt.Errorf("outpoint %v does not match ark tx %v",
			req.Outpoint, req.ArkTx.TxHash())

	case int(req.Outpoint.Index) >= len(req.ArkTx.TxOut):
		return fmt.Errorf("outpoint index %d out of bounds for ark tx",
			req.Outpoint.Index)

	case req.AssetAmount == 0:
		return fmt.Errorf("asset amount must be positive")

	case req.AssetRoot == (chainhash.Hash{}):
		return fmt.Errorf("asset root must be non-zero")

	case len(req.SealedArkPackage) == 0:
		return fmt.Errorf("sealed ark asset package is required")
	}

	ref, err := tapsdk.ParseAssetRef(req.AssetRef)
	if err != nil {
		return fmt.Errorf("asset VTXO reference: %w", err)
	}

	committed, err := decodeSealedPackage(req.SealedArkPackage)
	if err != nil {
		return err
	}

	anchor, err := psbtutil.Parse(committed.anchorPSBT)
	if err != nil {
		return fmt.Errorf("decode sealed ark transaction: %w", err)
	}
	if anchor.UnsignedTx.TxHash() != req.ArkTx.TxHash() {
		return fmt.Errorf("sealed package does not seal ark tx %v",
			req.ArkTx.TxHash())
	}

	targetOutpoint := sdkOutpoint(req.Outpoint)
	expectedRoot := tapsdk.Hash(req.AssetRoot)

	var (
		matchingOutputs []*commitOutput
		total           uint64
	)
	for idx := range committed.outputs {
		output := &committed.outputs[idx]
		if output.anchorOutpoint != targetOutpoint {
			continue
		}

		switch {
		case !output.assetRef.Equivalent(ref):
			return fmt.Errorf("asset output %d holds %s, want %s",
				idx, output.assetRef, ref)

		case output.scriptMode != customanchor.AssetScriptOPTrue ||
			len(output.opTrueWitness) == 0:
			return fmt.Errorf("asset output %d is not "+
				"OP_TRUE locked", idx)

		case output.taprootAssetRoot != expectedRoot:
			return fmt.Errorf("asset output %d does not commit to "+
				"the expected asset root", idx)

		case output.amount > math.MaxUint64-total:
			return fmt.Errorf("asset output amount overflow")
		}

		total += output.amount
		matchingOutputs = append(matchingOutputs, output)
	}

	if len(matchingOutputs) == 0 {
		return fmt.Errorf("sealed ark package holds no asset in %v",
			req.Outpoint)
	}
	if total != req.AssetAmount {
		return fmt.Errorf("sealed ark package holds %d units in "+
			"%v, want %d", total, req.Outpoint, req.AssetAmount)
	}

	packages := make([]OORPackageTxs, 0, len(req.AncestorPackages)+1)
	packages = append(packages, req.AncestorPackages...)
	packages = append(packages, OORPackageTxs{
		ArkTx:         req.ArkTx,
		CheckpointTxs: req.CheckpointTxs,
	})

	proofVerifier, err := NewOORProofVerifier(
		v.chain, req.Lineage, packages,
	)
	if err != nil {
		return err
	}

	for idx, output := range matchingOutputs {
		claimInput, err := claimInputFromOutput(committed, output)
		if err != nil {
			return fmt.Errorf("asset output %d claim input: %w",
				idx, err)
		}
		if err := claimInput.ProofPath.Validate(); err != nil {
			return fmt.Errorf("asset output %d proof path: %w", idx,
				err)
		}

		summary, err := claimInput.ProofPath.Verify(ctx, proofVerifier)
		if err != nil {
			return fmt.Errorf("verify asset output %d proof "+
				"path: %w", idx, err)
		}
		if err := checkProvenOutput(summary, output, ref); err != nil {
			return fmt.Errorf("asset output %d proven output: %w",
				idx, err)
		}
	}

	return nil
}

var (
	_ customanchor.ConfirmedProofVerifier    = (*oorProofVerifier)(nil)
	_ customanchor.UnconfirmedAnchorVerifier = (*oorProofVerifier)(nil)
)
