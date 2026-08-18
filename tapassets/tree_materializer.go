package tapassets

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
)

// TreeLeafAnchor describes how one leaf VTXO output is built: the
// uncomposed policy script the Bitcoin template carries before tapd grafts
// the asset commitment in, and the policy's tapscript material tapd needs
// to compose the final anchor key.
type TreeLeafAnchor struct {
	// UncomposedPkScript is the leaf policy's P2TR script without any
	// asset commitment.
	UncomposedPkScript []byte

	// InternalKey is the leaf policy's taproot internal key.
	InternalKey *btcec.PublicKey

	// TapLeaves are the complete policy leaves, in canonical order.
	TapLeaves []txscript.TapLeaf
}

// TreeRootAssetSource identifies the asset spent by the tree's root node:
// the batch output's asset units. The proof source describes the batch
// anchor (a confirmed proof file, or a compact path when the batch output
// itself is still unconfirmed), and the witness is the batch output's
// OP_TRUE asset witness.
type TreeRootAssetSource struct {
	// ProofFile is a complete confirmed proof file for the batch
	// anchor. Exactly one of ProofFile and ProofPath must be set.
	ProofFile []byte

	// ProofPath is a serialized compact proof path for an unconfirmed
	// batch anchor.
	ProofPath []byte

	// Witness is the caller-provided asset witness stack authorizing
	// the batch output's asset spend. Empty selects tapd's backend
	// signer, for batch outputs whose asset script key is wallet-owned.
	Witness [][]byte

	// Verifier verifies the proof source when building each commit.
	Verifier tapsdk.ConfirmedProofVerifier

	// SigningTweak is the batch output's combined taproot tweak, used
	// to sign the root node transaction.
	SigningTweak []byte

	// BatchPkScript is the batch output's final on-chain script, used
	// to bind the root node's key material fail-closed.
	BatchPkScript []byte

	// expectedSteps bind the unconfirmed steps already inside ProofPath
	// to their exact anchor transactions, so child paths extend from
	// the right depth. Populated by BatchAnchorCommitter.
	expectedSteps []*expectedUnconfirmedAnchor

	// proofPath is the in-process form of ProofPath, set by
	// BatchAnchorCommitter to skip a redundant decode-validate cycle.
	// It wins over the serialized fields when present.
	proofPath *tapsdk.AssetProofPath
}

// TreeMaterializerConfig configures asset tree materialization.
type TreeMaterializerConfig struct {
	// Wallet is the tap-sdk wallet used to commit node transitions.
	Wallet *tapsdk.Wallet

	// AssetRef is the asset carried by every leaf of this tree.
	AssetRef tapsdk.AssetRef

	// AssetContext receives per-node signing tweaks and sealed
	// packages, keyed by node input outpoint. Its subtree amounts must
	// already be populated by the structure pass.
	AssetContext *tree.AssetTreeContext

	// SweepLeaf is the operator's timeout sweep leaf carried by every
	// branch output as the asset commitment's tapscript sibling.
	SweepLeaf txscript.TapLeaf

	// LeafAnchor returns the anchor material for a leaf node.
	LeafAnchor func(node *tree.Node) (TreeLeafAnchor, error)

	// Root identifies the asset spent by the root node.
	Root TreeRootAssetSource

	// Digest scopes the deterministic OP_TRUE script keys of this
	// tree's outputs.
	Digest tapsdk.Hash
}

// treePathVerifier binds every unconfirmed step of a node's compact proof
// path to the exact ancestor transaction the materializer committed.
// Unlike the OOR verifiers, which bind only the newest step, tree paths
// grow one step per level and every level is materializer-authored, so
// each one is checked byte-for-byte.
type treePathVerifier struct {
	base  tapsdk.ConfirmedProofVerifier
	steps []*expectedUnconfirmedAnchor
}

// VerifyConfirmedProof delegates the confirmed base to the batch anchor's
// verifier.
func (v *treePathVerifier) VerifyConfirmedProof(ctx context.Context,
	proofFile []byte) (*tapsdk.ConfirmedProofVerification, error) {

	return v.base.VerifyConfirmedProof(ctx, proofFile)
}

// VerifyUnconfirmedAnchor requires the step to match the recorded ancestor
// transaction at its exact position.
func (v *treePathVerifier) VerifyUnconfirmedAnchor(_ context.Context,
	transition tapsdk.UnconfirmedAnchorVerification) error {

	if int(transition.StepIndex) >= len(v.steps) {
		return fmt.Errorf("unexpected unconfirmed proof step %d",
			transition.StepIndex)
	}
	expected := v.steps[transition.StepIndex]
	if transition.PreviousAnchorOutpoint != expected.previousOutpoint {
		return fmt.Errorf("step %d previous outpoint mismatch",
			transition.StepIndex)
	}
	if transition.AnchorOutpoint != expected.anchorOutpoint {
		return fmt.Errorf("step %d anchor outpoint mismatch",
			transition.StepIndex)
	}
	if !bytes.Equal(transition.AnchorTransaction, expected.transaction) {
		return fmt.Errorf("step %d anchor transaction mismatch",
			transition.StepIndex)
	}

	return nil
}

// nodeAssetHandoff carries everything a child node commit needs from its
// materialized parent.
type nodeAssetHandoff struct {
	source       *assetSpendSource
	steps        []*expectedUnconfirmedAnchor
	amount       uint64
	signingTweak tapsdk.Hash
	pkScript     []byte
}

// TreeMaterializer implements tree.Materializer for asset-aware trees: it
// commits one tap-sdk custom-anchor transition per node, top-down, letting
// tapd graft the asset commitment into every output key while this side
// pins the Bitcoin topology byte-for-byte.
type TreeMaterializer struct {
	cfg      TreeMaterializerConfig
	driver   customAnchorDriver
	handoffs map[wire.OutPoint]*nodeAssetHandoff

	// currentSteps are the expected unconfirmed anchors of the node
	// being materialized, extended per child in prepareChildren.
	currentSteps []*expectedUnconfirmedAnchor
}

// NewTreeMaterializer returns a materializer for one asset tree.
func NewTreeMaterializer(cfg TreeMaterializerConfig) (*TreeMaterializer,
	error) {

	if cfg.Wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}
	if cfg.AssetContext == nil {
		return nil, fmt.Errorf("asset tree context is required")
	}
	if cfg.LeafAnchor == nil {
		return nil, fmt.Errorf("leaf anchor resolver is required")
	}
	if len(cfg.SweepLeaf.Script) == 0 {
		return nil, fmt.Errorf("sweep leaf is required")
	}

	return &TreeMaterializer{
		cfg: cfg,
		driver: &sdkDriver{
			wallet: cfg.Wallet,
		},
		handoffs: make(map[wire.OutPoint]*nodeAssetHandoff),
	}, nil
}

// MaterializeNode commits the asset transition of one tree node and fills
// in its Bitcoin transaction data.
func (m *TreeMaterializer) MaterializeNode(ctx context.Context, node *tree.Node,
	params tree.MaterializeParams) (map[uint32]tree.MaterializeParams,
	error) {

	node.Input = params.Input

	source, amount, tweak, spentPkScript, err := m.resolveInput(
		params.Input,
	)
	if err != nil {
		return nil, err
	}

	// Bind the node's key material to the output it spends before any
	// external call: the MuSig2 aggregate of the node's cosigners,
	// tweaked with the recorded signing tweak, must reproduce the spent
	// output's script exactly.
	finalKey, err := boundFinalKey(node.CoSigners, tweak, spentPkScript)
	if err != nil {
		return nil, fmt.Errorf("node %s (%d children): %w",
			params.Input, len(node.Children), err)
	}

	template, outputSpecs, err := m.buildTemplate(node)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", params.Input, err)
	}

	committed, err := m.commitNode(
		ctx, node, params.Input, source, amount, template, outputSpecs,
	)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", params.Input, err)
	}

	committedTx, err := m.validateCommit(
		params.Input, amount, template, outputSpecs, committed,
	)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", params.Input, err)
	}

	// The committed transaction is authoritative: its outputs carry the
	// asset commitments every descendant binds to.
	node.Outputs = committedTx.TxOut
	node.FinalKey = finalKey

	m.cfg.AssetContext.SetSigningTweak(params.Input, tweak)
	m.cfg.AssetContext.SetSealedPackage(
		params.Input, committed.packageBytes,
	)

	// A leaf node's single output is the VTXO itself, so record its
	// asset commitment root: the owner needs it to reproduce the
	// composed script it was paid to, and to know the VTXO carries
	// assets at all. Branch outputs commit to their children instead.
	if len(node.Children) == 0 && len(committed.outputs) == 1 {
		root := committed.outputs[0].taprootAssetRoot
		m.cfg.AssetContext.SetLeafAssetRoot(params.Input, root[:])
	}

	// Re-register the subtree total now that the node carries its
	// input, so the amount stays resolvable on extracted or
	// deserialized clones. The structure pass's node-keyed total is
	// authoritative: the resolved input amount is zero for the root,
	// whose amount the proof verifier authenticates at commit time,
	// and writing that zero would clobber a single-leaf tree's only
	// node.
	m.cfg.AssetContext.SetNodeAssetAmount(
		node, m.cfg.AssetContext.NodeAssetAmount(node),
	)

	return m.prepareChildren(
		node, params.Input, source, committedTx, committed, outputSpecs,
	)
}

// resolveInput returns the asset spend source and binding material for the
// node transaction spending the given outpoint.
func (m *TreeMaterializer) resolveInput(input wire.OutPoint) (*assetSpendSource,
	uint64, []byte, []byte, error) {

	if handoff, ok := m.handoffs[input]; ok {
		delete(m.handoffs, input)
		m.currentSteps = handoff.steps

		return handoff.source, handoff.amount,
			handoff.signingTweak[:], handoff.pkScript, nil
	}
	m.currentSteps = m.cfg.Root.expectedSteps

	// No handoff means this is the root node spending the batch output.
	root := m.cfg.Root
	if len(root.SigningTweak) == 0 || len(root.BatchPkScript) == 0 {
		return nil, 0, nil, nil, fmt.Errorf("root asset source is " +
			"incomplete")
	}

	// An empty witness stack selects the backend signer: the batch
	// output's asset script key is a tapd wallet key, so tapd signs the
	// root transition itself. Tree-internal outputs are OP_TRUE and
	// always hand their witness stacks forward explicitly.
	witnessPlan := tapsdk.CustomAssetWitnessPlan{
		Mode: witnessBackendSigner,
	}
	if len(root.Witness) != 0 {
		witnessPlan = tapsdk.CustomAssetWitnessPlan{
			Mode:  witnessCallerProvided,
			Stack: cloneByteSlices(root.Witness),
		}
	}
	source := &assetSpendSource{
		witness:  witnessPlan,
		verifier: root.Verifier,
	}
	switch {
	case root.proofPath != nil:
		source.proofPath = root.proofPath.Clone()

	case len(root.ProofFile) != 0:
		source.proofFile = append([]byte(nil), root.ProofFile...)

	case len(root.ProofPath) != 0:
		var path tapsdk.AssetProofPath
		if err := path.UnmarshalBinary(root.ProofPath); err != nil {
			return nil, 0, nil, nil, fmt.Errorf("decode root "+
				"proof path: %w", err)
		}
		source.proofPath = &path

	default:
		return nil, 0, nil, nil, fmt.Errorf("root proof source is " +
			"empty")
	}

	// The root input's asset amount is authenticated by the proof
	// verifier at commit time; children instead carry their parent's
	// authenticated output amount, checked against the output total.
	return source, 0, root.SigningTweak, root.BatchPkScript, nil
}

// buildTemplate constructs the node's Bitcoin transaction template with
// uncomposed output scripts, and describes each asset output for the
// commit request.
func (m *TreeMaterializer) buildTemplate(node *tree.Node) (*psbt.Packet,
	[]treeOutputSpec, error) {

	indices := sortedChildIndices(node.Children)

	tx := wire.NewMsgTx(3)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: node.Input,
		Sequence:         node.TxSequence(),
	})

	var specs []treeOutputSpec
	if len(indices) == 0 {
		anchor, err := m.cfg.LeafAnchor(node)
		if err != nil {
			return nil, nil, err
		}
		leafAmount := m.cfg.AssetContext.NodeAssetAmount(node)
		if leafAmount == 0 {
			return nil, nil, fmt.Errorf("leaf carries no asset " +
				"amount")
		}

		tx.AddTxOut(
			wire.NewTxOut(
				int64(node.Amount), anchor.UncomposedPkScript,
			),
		)
		specs = append(specs, treeOutputSpec{
			index:       0,
			assetAmount: leafAmount,
			btcValue:    int64(node.Amount),
			internalKey: anchor.InternalKey,
			tapLeaves:   anchor.TapLeaves,
		})
	}

	for _, idx := range indices {
		child := node.Children[idx]
		childAmount := m.cfg.AssetContext.NodeAssetAmount(child)
		if childAmount == 0 {
			return nil, nil, fmt.Errorf("child %d carries no "+
				"asset amount", idx)
		}

		internalKey, err := tree.ComputeInternalKey(child.CoSigners)
		if err != nil {
			return nil, nil, err
		}
		uncomposed, err := tree.ComputeFinalKey(
			child.CoSigners, m.sweepRoot(),
		)
		if err != nil {
			return nil, nil, err
		}
		uncomposedScript, err := txscript.PayToTaprootScript(
			uncomposed,
		)
		if err != nil {
			return nil, nil, err
		}

		tx.AddTxOut(
			wire.NewTxOut(
				int64(child.Amount), uncomposedScript,
			),
		)
		specs = append(specs, treeOutputSpec{
			index:       idx,
			assetAmount: childAmount,
			btcValue:    int64(child.Amount),
			internalKey: internalKey,
			tapLeaves:   []txscript.TapLeaf{m.cfg.SweepLeaf},
		})
	}

	// Zero-fee node transactions carry a trailing P2A output for
	// broadcast-time fee bumping.
	tx.AddTxOut(arkscript.AnchorOutput())

	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, nil, err
	}

	return packet, specs, nil
}

// treeOutputSpec describes one asset output of a node transaction.
type treeOutputSpec struct {
	index       uint32
	assetAmount uint64
	btcValue    int64
	internalKey *btcec.PublicKey
	tapLeaves   []txscript.TapLeaf
}

// sweepRoot returns the tap hash of the operator sweep leaf.
func (m *TreeMaterializer) sweepRoot() []byte {
	hash := m.cfg.SweepLeaf.TapHash()

	return hash[:]
}

// commitNode assembles and commits the node's custom-anchor request.
func (m *TreeMaterializer) commitNode(ctx context.Context, node *tree.Node,
	input wire.OutPoint, source *assetSpendSource, amount uint64,
	template *psbt.Packet, specs []treeOutputSpec) (*commitResult, error) {

	anchorBytes, err := psbtutil.Serialize(template)
	if err != nil {
		return nil, err
	}

	sessionContext := []byte(input.String())

	// The node input's total asset amount is the sum of its outputs:
	// tree transitions never burn or mint.
	var total uint64
	for _, spec := range specs {
		total += spec.assetAmount
	}
	if amount != 0 && amount != total {
		return nil, fmt.Errorf("input amount %d does not match output "+
			"total %d", amount, total)
	}

	assetInput, err := source.customInput(
		"wavelength-tree-node", m.cfg.AssetRef, total,
	)
	if err != nil {
		return nil, err
	}

	outputs := make([]tapsdk.CustomAssetOutput, len(specs))
	for i, spec := range specs {
		opTrueKey := deterministicKey(
			m.cfg.Digest, fmt.Sprintf("tree/%s/%d", input,
				spec.index),
		)
		outputs[i] = tapsdk.CustomAssetOutput{
			ID: fmt.Sprintf(
				"wavelength-tree-out-%d", spec.index,
			),
			AssetRef:          m.cfg.AssetRef,
			Amount:            spec.assetAmount,
			AnchorOutputIndex: spec.index,
			AnchorValueSat:    uint64(spec.btcValue),
			Script: tapsdk.CustomAssetScriptPlan{
				Mode: tapsdk.CustomAssetScriptOPTrue,
				OPTrue: &tapsdk.CustomAssetOPTrueScriptPlan{
					InternalKey: tapsdk.KeyDescriptor{
						RawKeyBytes: opTrueKey,
					},
				},
			},
			Anchor: anchorPlan(spec.internalKey, spec.tapLeaves),
		}
	}

	request := &tapsdk.CustomAnchorRequest{
		Inputs: []tapsdk.CustomAssetInput{
			assetInput,
		},
		Outputs:    outputs,
		AnchorPSBT: anchorBytes,
		Funding:    callerFundedExact(),
		PassiveAssets: tapsdk.CustomAnchorPassiveAssets{
			Policy: tapsdk.CustomAnchorPassiveReject,
		},
		LossPolicy: tapsdk.CustomAnchorLossPolicy{
			Mode: tapsdk.CustomAnchorLossReject,
		},
		SigningPlans: []tapsdk.CustomAnchorInputSigningPlan{
			musig2SigningPlan(0, node.CoSigners, sessionContext),
		},
	}

	return m.driver.Commit(ctx, request, source.verifier)
}

// validateCommit binds the sealed result to the exact template: the
// committed transaction may differ only in the asset outputs' scripts,
// which must reproduce from each output's internal key and authenticated
// merkle root.
func (m *TreeMaterializer) validateCommit(input wire.OutPoint, amount uint64,
	template *psbt.Packet, specs []treeOutputSpec,
	committed *commitResult) (*wire.MsgTx, error) {

	if committed.fundingMode !=
		tapsdk.CustomAnchorFundingCallerFundedExact {
		return nil, fmt.Errorf("unexpected funding mode %d",
			committed.fundingMode)
	}
	if committed.actualFeeSat != 0 {
		return nil, fmt.Errorf("node transactions must be zero "+
			"fee, got %d", committed.actualFeeSat)
	}
	if len(committed.inputs) != 1 {
		return nil, fmt.Errorf("expected one asset input, got %d",
			len(committed.inputs))
	}
	if committed.inputs[0].anchorOutpoint != sdkOutpoint(input) {
		return nil, fmt.Errorf("asset input outpoint mismatch")
	}
	if len(committed.outputs) != len(specs) {
		return nil, fmt.Errorf("expected %d asset outputs, got %d",
			len(specs), len(committed.outputs))
	}

	arkTx, err := psbtutil.Parse(committed.anchorPSBT)
	if err != nil {
		return nil, err
	}
	committedTx := arkTx.UnsignedTx

	// Rebuild the expected transaction from the template by patching
	// only the asset outputs' scripts with the committed composed
	// scripts, then require byte equality: any other divergence means
	// the backend changed topology behind our back.
	expected := template.UnsignedTx.Copy()
	byIndex := make(map[uint32]*commitOutput, len(committed.outputs))
	for i := range committed.outputs {
		out := &committed.outputs[i]
		byIndex[out.anchorOutputIndex] = out
	}

	for _, spec := range specs {
		out, ok := byIndex[spec.index]
		if !ok {
			return nil, fmt.Errorf("committed result misses "+
				"output %d", spec.index)
		}
		if out.amount != spec.assetAmount {
			return nil, fmt.Errorf("output %d asset amount "+
				"%d, want %d", spec.index, out.amount,
				spec.assetAmount)
		}
		if out.anchorValueSat != spec.btcValue {
			return nil, fmt.Errorf("output %d carrier value "+
				"%d, want %d", spec.index, out.anchorValueSat,
				spec.btcValue)
		}
		if out.scriptMode != tapsdk.CustomAssetScriptOPTrue {
			return nil, fmt.Errorf("output %d script mode %d is "+
				"not OP_TRUE", spec.index, out.scriptMode)
		}
		if len(out.opTrueWitness) == 0 || len(out.proofBlob) == 0 {
			return nil, fmt.Errorf("output %d misses witness or "+
				"proof material", spec.index)
		}
		if out.taprootMerkleRoot == (tapsdk.Hash{}) {
			return nil, fmt.Errorf("output %d misses its "+
				"merkle root", spec.index)
		}

		// The composed script must reproduce from the declared
		// internal key and the authenticated combined root.
		composedKey := txscript.ComputeTaprootOutputKey(
			spec.internalKey, out.taprootMerkleRoot[:],
		)
		composedScript, err := txscript.PayToTaprootScript(
			composedKey,
		)
		if err != nil {
			return nil, err
		}
		if int(spec.index) >= len(committedTx.TxOut) {
			return nil, fmt.Errorf("committed tx misses output %d",
				spec.index)
		}
		if !bytes.Equal(
			committedTx.TxOut[spec.index].PkScript, composedScript,
		) {
			return nil, fmt.Errorf("output %d script does not "+
				"reproduce from its merkle root", spec.index)
		}

		expected.TxOut[spec.index].PkScript = composedScript
	}

	var expectedBuf, committedBuf bytes.Buffer
	if err := expected.Serialize(&expectedBuf); err != nil {
		return nil, err
	}
	if err := committedTx.Serialize(&committedBuf); err != nil {
		return nil, err
	}
	if !bytes.Equal(expectedBuf.Bytes(), committedBuf.Bytes()) {
		return nil, fmt.Errorf("committed transaction diverges from " +
			"the node template")
	}

	return committedTx, nil
}

// prepareChildren derives each child's spend source and binding material
// from the committed parent.
func (m *TreeMaterializer) prepareChildren(node *tree.Node, input wire.OutPoint,
	source *assetSpendSource, committedTx *wire.MsgTx,
	committed *commitResult, specs []treeOutputSpec) (
	map[uint32]tree.MaterializeParams, error) {

	childParams := make(
		map[uint32]tree.MaterializeParams, len(node.Children),
	)
	if len(node.Children) == 0 {
		return childParams, nil
	}

	byIndex := make(map[uint32]*commitOutput, len(committed.outputs))
	for i := range committed.outputs {
		out := &committed.outputs[i]
		byIndex[out.anchorOutputIndex] = out
	}

	serializedTx := serializeTx(committedTx)
	txid := committedTx.TxHash()

	for idx := range node.Children {
		out, ok := byIndex[idx]
		if !ok {
			return nil, fmt.Errorf("committed result misses child "+
				"output %d", idx)
		}

		childInput := wire.OutPoint{Hash: txid, Index: idx}
		expected := &expectedUnconfirmedAnchor{
			previousOutpoint: sdkOutpoint(input),
			anchorOutpoint:   out.anchorOutpoint,
			transaction:      append([]byte(nil), serializedTx...),
		}
		transitionInput, err := source.appendTransition(
			out.proofBlob, out.opTrueWitness,
		)
		if err != nil {
			return nil, fmt.Errorf("child %d: %w", idx, err)
		}

		// The child's path carries every ancestor transition, so
		// its verifier binds every step, not only the newest one.
		childSteps := make(
			[]*expectedUnconfirmedAnchor, len(m.currentSteps),
			len(m.currentSteps)+1,
		)
		copy(childSteps, m.currentSteps)
		childSteps = append(childSteps, expected)

		m.handoffs[childInput] = &nodeAssetHandoff{
			source: &assetSpendSource{
				proofPath: transitionInput.ProofPath,
				witness:   transitionInput.Witness,
				verifier: &treePathVerifier{
					base:  m.cfg.Root.Verifier,
					steps: childSteps,
				},
			},
			steps:        childSteps,
			amount:       out.amount,
			signingTweak: out.taprootMerkleRoot,
			pkScript: append(
				[]byte(nil), committedTx.TxOut[idx].PkScript...,
			),
		}
		childParams[idx] = tree.MaterializeParams{Input: childInput}
	}

	return childParams, nil
}

// boundFinalKey recomputes the spent output's key from the node's
// cosigners and signing tweak and requires it to reproduce the spent
// output's script exactly.
func boundFinalKey(cosigners []*btcec.PublicKey, tweak,
	spentPkScript []byte) (*btcec.PublicKey, error) {

	internalKey, err := tree.ComputeInternalKey(cosigners)
	if err != nil {
		return nil, err
	}
	finalKey := txscript.ComputeTaprootOutputKey(internalKey, tweak)

	script, err := txscript.PayToTaprootScript(finalKey)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(script, spentPkScript) {
		keys := make([]string, len(cosigners))
		for i, cosigner := range cosigners {
			keys[i] = hex.EncodeToString(
				cosigner.SerializeCompressed(),
			)
		}

		return nil, fmt.Errorf("cosigners [%s] and signing tweak %x "+
			"derive script %x, but the spent output is %x",
			strings.Join(keys, " "), tweak, script, spentPkScript)
	}

	return finalKey, nil
}

// musig2SigningPlan declares the MuSig2 aggregate key spend of one anchor
// input. The session context distinguishes signing sessions; the spent
// outpoint is unique per node and public by construction.
func musig2SigningPlan(index uint32, cosigners []*btcec.PublicKey,
	sessionContext []byte) tapsdk.CustomAnchorInputSigningPlan {

	// The aggregate internal key sorts cosigners before aggregation, and
	// the backend aggregates participants in declared order, so declare
	// them pre-sorted for the two derivations to agree.
	sorted := make([]*btcec.PublicKey, len(cosigners))
	copy(sorted, cosigners)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(
			sorted[i].SerializeCompressed(),
			sorted[j].SerializeCompressed(),
		) < 0
	})

	participants := make([]tapsdk.PubKey, 0, len(sorted))
	for _, cosigner := range sorted {
		key, _ := tapsdk.ParsePubKey(cosigner.SerializeCompressed())
		participants = append(participants, key)
	}

	return tapsdk.CustomAnchorInputSigningPlan{
		InputIndex: index,
		MuSig2: &tapsdk.CustomAnchorMuSig2SigningPlan{
			Participants:   participants,
			SessionContext: sessionContext,
		},
	}
}

// sortedChildIndices returns a node's child indices in ascending order.
func sortedChildIndices(children map[uint32]*tree.Node) []uint32 {
	indices := make([]uint32, 0, len(children))
	for idx := range children {
		indices = append(indices, idx)
	}
	sort.Slice(indices, func(i, j int) bool {
		return indices[i] < indices[j]
	})

	return indices
}
