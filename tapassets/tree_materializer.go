package tapassets

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
)

// TreeLeafAnchor contains the taproot data for a leaf VTXO output.
type TreeLeafAnchor struct {
	// UncomposedPkScript is the policy script before adding the asset root.
	UncomposedPkScript []byte

	// InternalKey is the leaf policy's taproot internal key.
	InternalKey *btcec.PublicKey

	// TapLeaves are the complete policy leaves, in canonical order.
	TapLeaves []txscript.TapLeaf
}

// TreeRootAssetInput identifies one asset state in the batch output.
type TreeRootAssetInput struct {
	// ProofFile proves a confirmed asset state. Exactly one of ProofFile
	// and ProofPath must be set.
	ProofFile []byte

	// ProofPath proves an asset state with unconfirmed transitions.
	ProofPath []byte

	// Amount is the number of asset units in this state.
	Amount uint64

	// AnchorOutpoint is the batch output containing the asset state.
	AnchorOutpoint wire.OutPoint

	// Witness authorizes the asset spend. An empty stack asks tapd to sign.
	Witness [][]byte

	// Verifier verifies this input's proof.
	Verifier tapsdk.ConfirmedProofVerifier
}

// TreeRootAssetSource identifies the asset states spent by the root node.
type TreeRootAssetSource struct {
	// Inputs are the asset states held by the batch output.
	Inputs []TreeRootAssetInput

	// SigningTweak is the batch output's taproot tweak.
	SigningTweak []byte

	// BatchPkScript is the batch output's P2TR script.
	BatchPkScript []byte
}

// TreeMaterializerConfig configures asset tree materialization.
type TreeMaterializerConfig struct {
	// Wallet is the tap-sdk wallet used to commit node transitions.
	Wallet *tapsdk.Wallet

	// Store caches completed commits for replay after a restart.
	Store Store

	// AssetRef identifies the one asset or asset group carried by this
	// tree.
	AssetRef tapsdk.AssetRef

	// SweepLeaf is the operator's timeout path.
	SweepLeaf txscript.TapLeaf

	// LeafAnchor returns the anchor material for a leaf node.
	LeafAnchor func(node *tree.Node) (TreeLeafAnchor, error)

	// Root identifies the asset spent by the root node.
	Root TreeRootAssetSource

	// Digest scopes the deterministic asset script keys.
	Digest tapsdk.Hash
}

// treePathVerifier delegates existing proof steps and checks appended tree
// steps against their transactions.
type treePathVerifier struct {
	base      tapsdk.ConfirmedProofVerifier
	baseDepth uint16
	steps     []*expectedUnconfirmedAnchor
}

// VerifyConfirmedProof verifies the confirmed base proof.
func (v *treePathVerifier) VerifyConfirmedProof(ctx context.Context,
	proofFile []byte) (*tapsdk.ConfirmedProofVerification, error) {

	if v == nil || v.base == nil {
		return nil, fmt.Errorf("confirmed asset proof verifier is " +
			"required")
	}

	return v.base.VerifyConfirmedProof(ctx, proofFile)
}

// VerifyUnconfirmedAnchor verifies one unconfirmed transition.
func (v *treePathVerifier) VerifyUnconfirmedAnchor(ctx context.Context,
	transition tapsdk.UnconfirmedAnchorVerification) error {

	if transition.StepIndex < v.baseDepth {
		base, ok := v.base.(tapsdk.UnconfirmedAnchorVerifier)
		if !ok {
			return fmt.Errorf("proof verifier cannot verify " +
				"unconfirmed step")
		}

		return base.VerifyUnconfirmedAnchor(ctx, transition)
	}

	stepIndex := int(transition.StepIndex - v.baseDepth)
	if stepIndex >= len(v.steps) {
		return fmt.Errorf("unexpected unconfirmed proof step %d",
			transition.StepIndex)
	}
	expected := v.steps[stepIndex]
	if expected == nil {
		return fmt.Errorf("unconfirmed proof step %d has no binding",
			transition.StepIndex)
	}
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
	if !slices.Equal(
		transition.PreviousAnchorOutpoints, expected.previousOutpoints,
	) {
		return fmt.Errorf("step %d input outpoints mismatch",
			transition.StepIndex)
	}

	return nil
}

type nodeAssetHandoff struct {
	sources      []*assetSpendSource
	amount       uint64
	signingTweak tapsdk.Hash
	pkScript     []byte
}

type treeMaterializer struct {
	cfg          TreeMaterializerConfig
	assetContext *tree.AssetTreeContext
	rootInput    wire.OutPoint
	driver       assetTreeDriver
	handoffs     map[wire.OutPoint]*nodeAssetHandoff
}

func newTreeMaterializer(cfg TreeMaterializerConfig,
	assetContext *tree.AssetTreeContext, rootInput wire.OutPoint,
	driver assetTreeDriver) (*treeMaterializer, error) {

	if cfg.Wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("asset transition store is required")
	}
	if assetContext == nil {
		return nil, fmt.Errorf("asset tree context is required")
	}
	if rootInput == (wire.OutPoint{}) {
		return nil, fmt.Errorf("root input is required")
	}
	if driver == nil {
		return nil, fmt.Errorf("asset tree driver is required")
	}
	if err := cfg.AssetRef.Validate(); err != nil {
		return nil, fmt.Errorf("asset ref: %w", err)
	}
	if cfg.LeafAnchor == nil {
		return nil, fmt.Errorf("leaf anchor resolver is required")
	}
	if len(cfg.SweepLeaf.Script) == 0 {
		return nil, fmt.Errorf("sweep leaf is required")
	}
	if cfg.Digest == (tapsdk.Hash{}) {
		return nil, fmt.Errorf("asset tree digest is required")
	}
	if len(cfg.Root.SigningTweak) != chainhash.HashSize {
		return nil, fmt.Errorf("root signing tweak must be %d bytes",
			chainhash.HashSize)
	}
	if !txscript.IsPayToTaproot(cfg.Root.BatchPkScript) {
		return nil, fmt.Errorf("batch output must be P2TR")
	}
	if len(cfg.Root.Inputs) == 0 {
		return nil, fmt.Errorf("root asset input is required")
	}
	for idx := range cfg.Root.Inputs {
		input := &cfg.Root.Inputs[idx]
		if err := validateRootAssetInput(
			*input, rootInput,
		); err != nil {
			return nil, fmt.Errorf("root asset input %d: %w", idx,
				err)
		}
	}

	return &treeMaterializer{
		cfg:          cfg,
		assetContext: assetContext,
		rootInput:    rootInput,
		driver:       driver,
		handoffs:     make(map[wire.OutPoint]*nodeAssetHandoff),
	}, nil
}

func validateRootAssetInput(input TreeRootAssetInput,
	rootInput wire.OutPoint) error {

	if input.Amount == 0 {
		return fmt.Errorf("amount is required")
	}
	if input.AnchorOutpoint != rootInput {
		return fmt.Errorf("anchor outpoint does not match the batch " +
			"output")
	}
	if input.Verifier == nil {
		return fmt.Errorf("proof verifier is required")
	}
	hasProofFile := len(input.ProofFile) != 0
	hasProofPath := len(input.ProofPath) != 0
	if hasProofFile == hasProofPath {
		return fmt.Errorf("exactly one proof source is required")
	}
	if !hasProofPath {
		return nil
	}

	var path tapsdk.AssetProofPath
	if err := path.UnmarshalBinary(input.ProofPath); err != nil {
		return fmt.Errorf("decode proof path: %w", err)
	}
	if len(path.Steps) != 0 {
		_, ok := input.Verifier.(tapsdk.UnconfirmedAnchorVerifier)
		if !ok {
			return fmt.Errorf("proof path requires an " +
				"unconfirmed anchor verifier")
		}
	}

	return nil
}

// MaterializeNode commits a node's asset transition.
func (m *treeMaterializer) MaterializeNode(ctx context.Context, node *tree.Node,
	params tree.MaterializeParams) (map[uint32]tree.MaterializeParams,
	error) {

	node.Input = params.Input

	sources, amount, tweak, spentPkScript, err := m.resolveInput(
		params.Input,
	)
	if err != nil {
		return nil, err
	}

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
		ctx, node, params.Input, sources, amount, template, outputSpecs,
	)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", params.Input, err)
	}

	committedTx, err := m.validateCommit(
		params.Input, sources, amount, template, outputSpecs, committed,
	)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", params.Input, err)
	}

	node.Outputs = committedTx.TxOut
	node.FinalKey = finalKey

	m.assetContext.SetSigningTweak(params.Input, tweak)
	m.assetContext.SetSealedPackage(
		params.Input, committed.packageBytes,
	)

	if len(node.Children) == 0 && len(committed.outputs) != 0 {
		root := committed.outputs[0].taprootAssetRoot
		m.assetContext.SetLeafAssetRoot(params.Input, root[:])
	}

	// Node inputs are assigned during materialization, so index the amount
	// under the finished node as well.
	m.assetContext.SetNodeAssetAmount(
		node, m.assetContext.NodeAssetAmount(node),
	)

	return m.prepareChildren(
		node, sources, committedTx, committed,
	)
}

func (m *treeMaterializer) resolveInput(input wire.OutPoint) (
	[]*assetSpendSource, uint64, []byte, []byte, error) {

	if handoff, ok := m.handoffs[input]; ok {
		delete(m.handoffs, input)

		return handoff.sources, handoff.amount,
			handoff.signingTweak[:], handoff.pkScript, nil
	}
	if input != m.rootInput {
		return nil, 0, nil, nil, fmt.Errorf("unknown tree input %s",
			input)
	}

	root := m.cfg.Root
	sources := make([]*assetSpendSource, len(root.Inputs))
	var total uint64
	for idx := range root.Inputs {
		input := root.Inputs[idx]
		if input.Amount > math.MaxUint64-total {
			return nil, 0, nil, nil, fmt.Errorf("root asset " +
				"amount overflow")
		}
		total += input.Amount

		witness := tapsdk.CustomAssetWitnessPlan{
			Mode: witnessBackendSigner,
		}
		if len(input.Witness) != 0 {
			witness = tapsdk.CustomAssetWitnessPlan{
				Mode:  witnessCallerProvided,
				Stack: cloneByteSlices(input.Witness),
			}
		}

		source := &assetSpendSource{
			witness:        witness,
			verifier:       input.Verifier,
			amount:         input.Amount,
			anchorOutpoint: sdkOutpoint(input.AnchorOutpoint),
		}
		if len(input.ProofFile) != 0 {
			source.proofFile = append(
				[]byte(nil), input.ProofFile...,
			)
		} else {
			var path tapsdk.AssetProofPath
			if err := path.UnmarshalBinary(
				input.ProofPath,
			); err != nil {
				return nil, 0, nil, nil, fmt.Errorf("decode "+
					"root proof path %d: %w", idx, err)
			}
			source.proofPath = &path
		}
		sources[idx] = source
	}

	return sources, total, root.SigningTweak, root.BatchPkScript, nil
}

// buildTemplate creates the Bitcoin template and its asset outputs.
func (m *treeMaterializer) buildTemplate(node *tree.Node) (*psbt.Packet,
	[]treeOutputSpec, error) {

	indices := sortedChildIndices(node.Children)

	tx := wire.NewMsgTx(3)
	tx.AddTxIn(wire.NewTxIn(&node.Input, nil, nil))

	var specs []treeOutputSpec
	if len(indices) == 0 {
		anchor, err := m.cfg.LeafAnchor(node)
		if err != nil {
			return nil, nil, err
		}
		leafAmount := m.assetContext.NodeAssetAmount(node)
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
		childAmount := m.assetContext.NodeAssetAmount(child)
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
func (m *treeMaterializer) sweepRoot() []byte {
	hash := m.cfg.SweepLeaf.TapHash()

	return hash[:]
}

func (m *treeMaterializer) commitNode(ctx context.Context, node *tree.Node,
	input wire.OutPoint, sources []*assetSpendSource, amount uint64,
	template *psbt.Packet, specs []treeOutputSpec) (*commitResult, error) {

	anchorBytes, err := psbtutil.Serialize(template)
	if err != nil {
		return nil, err
	}

	sessionContext := []byte(input.String())

	var total uint64
	for _, spec := range specs {
		if spec.assetAmount > math.MaxUint64-total {
			return nil, fmt.Errorf("asset output amount overflow")
		}
		total += spec.assetAmount
	}
	if amount != total {
		return nil, fmt.Errorf("input amount %d does not match output "+
			"total %d", amount, total)
	}

	inputs := make([]tapsdk.CustomAssetInput, len(sources))
	for idx := range sources {
		assetInput, err := sources[idx].customInput(
			fmt.Sprintf("wavelength-tree-input-%d", idx),
			m.cfg.AssetRef,
		)
		if err != nil {
			return nil, err
		}
		inputs[idx] = assetInput
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
		Inputs:     inputs,
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

	return m.commitDurably(ctx, input, request, &assetSourceVerifier{
		sources: sources,
	})
}

func (m *treeMaterializer) validateCommit(input wire.OutPoint,
	sources []*assetSpendSource, amount uint64, template *psbt.Packet,
	specs []treeOutputSpec, committed *commitResult) (*wire.MsgTx, error) {

	if committed == nil {
		return nil, fmt.Errorf("asset commit result is required")
	}
	if committed.fundingMode !=
		tapsdk.CustomAnchorFundingCallerFundedExact {
		return nil, fmt.Errorf("unexpected funding mode %d",
			committed.fundingMode)
	}
	if committed.actualFeeSat != 0 {
		return nil, fmt.Errorf("node transactions must be zero "+
			"fee, got %d", committed.actualFeeSat)
	}
	var expectedTotal uint64
	for _, spec := range specs {
		if spec.assetAmount > math.MaxUint64-expectedTotal {
			return nil, fmt.Errorf("asset output amount overflow")
		}
		expectedTotal += spec.assetAmount
	}
	if amount != expectedTotal {
		return nil, fmt.Errorf("asset input amount %d, want %d", amount,
			expectedTotal)
	}
	if err := m.validateCommitInputs(
		input, sources, committed.inputs,
	); err != nil {
		return nil, err
	}
	if err := validateIssuanceConservation(
		sources, committed.outputs,
	); err != nil {
		return nil, err
	}

	arkTx, err := psbtutil.Parse(committed.anchorPSBT)
	if err != nil {
		return nil, err
	}
	committedTx := arkTx.UnsignedTx

	expected := template.UnsignedTx.Copy()
	byIndex := make(map[uint32][]*commitOutput, len(specs))
	for i := range committed.outputs {
		out := &committed.outputs[i]
		byIndex[out.anchorOutputIndex] = append(
			byIndex[out.anchorOutputIndex], out,
		)
	}
	if len(byIndex) != len(specs) {
		return nil, fmt.Errorf("committed result has %d asset "+
			"outputs, want %d", len(byIndex), len(specs))
	}

	for _, spec := range specs {
		outputs := byIndex[spec.index]
		if len(outputs) == 0 {
			return nil, fmt.Errorf("committed result misses "+
				"output %d", spec.index)
		}
		if err := m.validateCommitOutputs(
			spec, committedTx, outputs,
		); err != nil {
			return nil, err
		}
		root := outputs[0].taprootMerkleRoot
		composedKey := txscript.ComputeTaprootOutputKey(
			spec.internalKey, root[:],
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

func validateIssuanceConservation(sources []*assetSpendSource,
	outputs []commitOutput) error {

	expected := make(map[tapsdk.AssetID]uint64, len(sources))
	for _, source := range sources {
		expected[source.issuanceID] = source.amount
	}
	actual := make(map[tapsdk.AssetID]uint64, len(sources))
	for _, output := range outputs {
		if _, ok := expected[output.issuanceID]; !ok {
			return fmt.Errorf("asset output has an unknown " +
				"issuance")
		}
		amount := actual[output.issuanceID]
		if output.amount > math.MaxUint64-amount {
			return fmt.Errorf("asset output amount overflow")
		}
		actual[output.issuanceID] = amount + output.amount
	}
	for issuanceID, want := range expected {
		if actual[issuanceID] != want {
			return fmt.Errorf("asset issuance amount %d, want %d",
				actual[issuanceID], want)
		}
	}

	return nil
}

func (m *treeMaterializer) validateCommitInputs(input wire.OutPoint,
	sources []*assetSpendSource, committed []commitInput) error {

	if len(committed) != len(sources) {
		return fmt.Errorf("asset commit has %d inputs, want %d",
			len(committed), len(sources))
	}
	byID := make(map[string]commitInput, len(committed))
	for idx := range committed {
		assetInput := committed[idx]
		if _, ok := byID[assetInput.logicalInputID]; ok {
			return fmt.Errorf("asset commit repeats input %q",
				assetInput.logicalInputID)
		}
		byID[assetInput.logicalInputID] = assetInput
	}

	for idx, source := range sources {
		id := fmt.Sprintf("wavelength-tree-input-%d", idx)
		assetInput, ok := byID[id]
		if !ok {
			return fmt.Errorf("asset commit misses input %q", id)
		}
		if assetInput.anchorOutpoint != sdkOutpoint(input) ||
			assetInput.anchorOutpoint != source.anchorOutpoint {
			return fmt.Errorf("asset input %d outpoint mismatch",
				idx)
		}
		if !assetInput.assetRef.Equivalent(m.cfg.AssetRef) {
			return fmt.Errorf("asset input %d ref mismatch", idx)
		}
		if source.issuanceKnown &&
			assetInput.issuanceID != source.issuanceID {
			return fmt.Errorf("asset input %d issuance mismatch",
				idx)
		}
		if !source.issuanceKnown {
			source.issuanceID = assetInput.issuanceID
			source.issuanceKnown = true
		}
		if assetInput.amount != source.amount {
			return fmt.Errorf("asset input %d amount %d, want %d",
				idx, assetInput.amount, source.amount)
		}
	}

	return nil
}

func (m *treeMaterializer) validateCommitOutputs(spec treeOutputSpec,
	committedTx *wire.MsgTx, outputs []*commitOutput) error {

	wantOutpoint := sdkOutpoint(wire.OutPoint{
		Hash:  committedTx.TxHash(),
		Index: spec.index,
	})
	wantAssetRoot := outputs[0].taprootAssetRoot
	wantMerkleRoot := outputs[0].taprootMerkleRoot
	issuances := make(map[tapsdk.AssetID]struct{}, len(outputs))
	var total uint64
	for _, output := range outputs {
		if output.amount > math.MaxUint64-total {
			return fmt.Errorf("output %d asset amount overflow",
				spec.index)
		}
		total += output.amount
		if !output.assetRef.Equivalent(m.cfg.AssetRef) {
			return fmt.Errorf("output %d asset ref mismatch",
				spec.index)
		}
		if _, ok := issuances[output.issuanceID]; ok {
			return fmt.Errorf("output %d repeats an issuance",
				spec.index)
		}
		issuances[output.issuanceID] = struct{}{}
		if output.anchorValueSat != spec.btcValue {
			return fmt.Errorf("output %d carrier value %d, want %d",
				spec.index, output.anchorValueSat,
				spec.btcValue)
		}
		if output.scriptMode != tapsdk.CustomAssetScriptOPTrue {
			return fmt.Errorf("output %d script mode %d is "+
				"not OP_TRUE", spec.index, output.scriptMode)
		}
		if len(output.opTrueWitness) == 0 ||
			len(output.proofBlob) == 0 {
			return fmt.Errorf("output %d misses witness or proof",
				spec.index)
		}
		if output.taprootAssetRoot == (tapsdk.Hash{}) ||
			output.taprootMerkleRoot == (tapsdk.Hash{}) {
			return fmt.Errorf("output %d misses a taproot root",
				spec.index)
		}
		if output.taprootAssetRoot != wantAssetRoot ||
			output.taprootMerkleRoot != wantMerkleRoot {
			return fmt.Errorf("output %d has inconsistent "+
				"taproot roots", spec.index)
		}
		if output.anchorOutpoint != wantOutpoint {
			return fmt.Errorf("output %d anchor outpoint mismatch",
				spec.index)
		}
	}
	if total != spec.assetAmount {
		return fmt.Errorf("output %d asset amount %d, want %d",
			spec.index, total, spec.assetAmount)
	}

	return nil
}

func (m *treeMaterializer) prepareChildren(node *tree.Node,
	sources []*assetSpendSource, committedTx *wire.MsgTx,
	committed *commitResult) (map[uint32]tree.MaterializeParams, error) {

	childParams := make(
		map[uint32]tree.MaterializeParams, len(node.Children),
	)
	if len(node.Children) == 0 {
		return childParams, nil
	}

	byIndex := make(map[uint32][]commitOutput, len(node.Children))
	for i := range committed.outputs {
		out := committed.outputs[i]
		byIndex[out.anchorOutputIndex] = append(
			byIndex[out.anchorOutputIndex], out,
		)
	}
	sourceByIssuance := make(
		map[tapsdk.AssetID]*assetSpendSource, len(sources),
	)
	for _, source := range sources {
		sourceByIssuance[source.issuanceID] = source
	}

	serializedTx := serializeTx(committedTx)
	txid := committedTx.TxHash()

	for idx := range node.Children {
		outputs := byIndex[idx]
		if len(outputs) == 0 {
			return nil, fmt.Errorf("committed result misses child "+
				"output %d", idx)
		}

		childInput := wire.OutPoint{Hash: txid, Index: idx}
		childSources := make([]*assetSpendSource, len(outputs))
		var childAmount uint64
		for outputIdx := range outputs {
			output := outputs[outputIdx]
			source := sourceByIssuance[output.issuanceID]
			if source == nil {
				return nil, fmt.Errorf("child %d has unknown "+
					"issuance", idx)
			}
			childSource, err := source.appendTransition(
				output, serializedTx,
			)
			if err != nil {
				return nil, fmt.Errorf("child %d: %w", idx, err)
			}
			childSources[outputIdx] = childSource
			if output.amount > math.MaxUint64-childAmount {
				return nil, fmt.Errorf("child %d asset amount "+
					"overflow", idx)
			}
			childAmount += output.amount
		}

		m.handoffs[childInput] = &nodeAssetHandoff{
			sources:      childSources,
			amount:       childAmount,
			signingTweak: outputs[0].taprootMerkleRoot,
			pkScript: append(
				[]byte(nil), committedTx.TxOut[idx].PkScript...,
			),
		}
		childParams[idx] = tree.MaterializeParams{Input: childInput}
	}

	return childParams, nil
}

// boundFinalKey checks the node keys against the output being spent.
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

// musig2SigningPlan returns the key-spend plan for an anchor input.
func musig2SigningPlan(index uint32, cosigners []*btcec.PublicKey,
	sessionContext []byte) tapsdk.CustomAnchorInputSigningPlan {

	// tree.ComputeInternalKey sorts cosigners before aggregation.
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
