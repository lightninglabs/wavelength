package tree

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/assets"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/assetwalletrpc"
	"google.golang.org/grpc"
)

// AssetWallet mirrors the subset of tapd's wallet client needed by the builder.
// We keep PublishAndLogTransfer to satisfy the builder interface, though
// FinalizeAssetNode no longer calls it directly.
type AssetWallet interface {
	CommitVirtualPsbts(context.Context,
		*assetwalletrpc.CommitVirtualPsbtsRequest, ...grpc.CallOption) (
		*assetwalletrpc.CommitVirtualPsbtsResponse, error)

	PublishAndLogTransfer(context.Context,
		*assetwalletrpc.PublishAndLogRequest, ...grpc.CallOption) (
		*taprpc.SendAssetResponse, error)
}

// FinalizeResult contains the signed anchor transaction and builder.
type FinalizeResult struct {
	// AnchorTx is the finalized anchor transaction. This should be broadcast
	// instead of the node's stored transaction because the proofs reference
	// this transaction's TXID.
	AnchorTx *wire.MsgTx

	// Builder is the AssetTxBuilder used for finalization. This is exposed
	// so callers can use builder.Proof() to generate proofs with correct
	// OP_TRUE witnesses after the transaction is mined.
	Builder *assets.AssetTxBuilder

	// TransferData captures the serialized virtual/anchor PSBTs so proofs
	// can be reconstructed after external broadcast.
	TransferData *assets.TransferData
}

// ChildOutputSpec describes a child output for a branch node. It contains the
// information needed to construct the output config: either an explicit anchor
// key or cosigners for computing the internal key, plus the asset amount.
type ChildOutputSpec struct {
	// CoSigners are the public keys participating in this child's MuSig2
	// aggregate. The internal key is computed from these. Used by the
	// rebuild path when the anchor key isn't stored directly.
	CoSigners []*btcec.PublicKey

	// AnchorKey optionally provides the child's anchor key directly. When
	// set, this is used instead of computing from CoSigners. Used by the
	// assembly path where the internal key is computed separately.
	AnchorKey *btcec.PublicKey

	// AssetAmount is the total asset amount for this child subtree.
	AssetAmount uint64
}

// NodeBuildSpec captures the information needed to construct an asset
// transaction for a tree node. It can be populated from either a stored Node
// (rebuild path) or from LeafDescriptors (assembly path).
type NodeBuildSpec struct {
	// Input is the outpoint being spent.
	Input wire.OutPoint

	// ParentProof is the serialized proof for the parent's output.
	ParentProof []byte

	// CoSigners are this node's cosigners (for computing the input's
	// internal key / anchor key). Used by the rebuild path.
	CoSigners []*btcec.PublicKey

	// InputAnchorKey optionally provides the input's anchor key directly.
	// When set, this overrides computing the anchor key from CoSigners.
	// Used by the assembly path where parentPlan.AnchorKey is available.
	InputAnchorKey *assets.AnchorKeySpec

	// IsLeaf indicates whether this is a leaf node.
	IsLeaf bool

	// LeafOwnerKey is the owner key for leaf nodes (non-operator cosigner).
	// Only used when IsLeaf is true.
	LeafOwnerKey *btcec.PublicKey

	// LeafAssetAmount is the asset amount for leaf nodes.
	// Only used when IsLeaf is true.
	LeafAssetAmount uint64

	// ChildOutputs describes each child output for branch nodes.
	// Only used when IsLeaf is false.
	ChildOutputs []ChildOutputSpec

	// AssetOutputValues maps output index to BTC value for each asset
	// output. Used by Commit to set output values so proofs reference the
	// correct txid. The ephemeral anchor output (value 0) is not included.
	AssetOutputValues map[uint32]int64
}

// AssetMaterializer builds asset transactions via tapd RPCs.
// It implements the Materializer interface for asset-aware trees.
type AssetMaterializer struct {
	// Config contains asset-level parameters.
	Config AssetTreeConfig

	// Wallet is the asset wallet client for committing transactions.
	Wallet assets.AssetWalletClient
}

// NewAssetMaterializer creates a new asset materializer.
func NewAssetMaterializer(cfg AssetTreeConfig,
	wallet assets.AssetWalletClient) *AssetMaterializer {

	return &AssetMaterializer{
		Config: cfg,
		Wallet: wallet,
	}
}

// MaterializeNode fills in transaction data for a single node using tapd RPCs
// to build the asset transaction.
func (m *AssetMaterializer) MaterializeNode(ctx context.Context, node *Node,
	params MaterializeParams) (map[uint32]MaterializeParams, error) {

	// Set the input outpoint.
	node.Input = params.Input

	if node.IsLeaf() {
		return m.materializeLeaf(ctx, node, params)
	}

	return m.materializeBranch(ctx, node, params)
}

// materializeLeaf builds the transaction for a leaf node.
func (m *AssetMaterializer) materializeLeaf(ctx context.Context, node *Node,
	params MaterializeParams) (map[uint32]MaterializeParams, error) {

	// Get leaf owner key (non-operator cosigner).
	ownerKey := GetLeafOwnerKey(node, m.Config.OperatorKey)
	if ownerKey == nil {
		return nil, fmt.Errorf("leaf node missing owner key")
	}

	// Build the spec for MakeAssetNodeTxBuilder.
	spec := &NodeBuildSpec{
		Input:           params.Input,
		ParentProof:     params.ParentProof,
		IsLeaf:          true,
		LeafOwnerKey:    ownerKey,
		LeafAssetAmount: GetLeafAssetAmount(node),
		AssetOutputValues: map[uint32]int64{
			0: params.InputBtcValue,
		},
	}

	// Use InputAnchorKey if parent plan is available (assembly path).
	if params.ParentPlan != nil {
		spec.InputAnchorKey = &params.ParentPlan.AnchorKey
	} else {
		// Rebuild path - compute from cosigners.
		spec.CoSigners = node.CoSigners
	}

	// Build the transaction.
	builder, _, err := MakeAssetNodeTxBuilder(ctx, m.Config, m.Wallet, spec)
	if err != nil {
		return nil, fmt.Errorf("build leaf tx: %w", err)
	}

	// Extract outputs from the anchor PSBT.
	anchorPsbt := builder.AnchorPsbt()
	if anchorPsbt == nil {
		return nil, fmt.Errorf("leaf anchor psbt missing")
	}

	anchorTx := anchorPsbt.UnsignedTx
	node.Outputs = make([]*wire.TxOut, len(anchorTx.TxOut))
	for i, out := range anchorTx.TxOut {
		node.Outputs[i] = wire.NewTxOut(out.Value, out.PkScript)
	}

	// Update WitnessUtxo value if needed.
	if len(anchorPsbt.Inputs) > 0 && anchorPsbt.Inputs[0].WitnessUtxo != nil {
		anchorPsbt.Inputs[0].WitnessUtxo.Value = params.InputBtcValue
	}

	// Extract proof suffix.
	proofBytes, err := firstProofSuffix(builder.ActivePackets())
	if err != nil {
		return nil, fmt.Errorf("leaf proof suffix: %w", err)
	}

	// Attach metadata.
	if node.Metadata != nil && node.Metadata.Leaf != nil {
		node.Metadata.Leaf.InputProof = proofBytes
	}

	// Attach taproot tweak from parent proof.
	if err := attachTaprootTweakFromParent(node, params.ParentPlan,
		params.ParentProof); err != nil {

		return nil, fmt.Errorf("attach leaf taproot tweak: %w", err)
	}

	// Leaves have no children.
	return nil, nil
}

// materializeBranch builds the transaction for a branch node.
func (m *AssetMaterializer) materializeBranch(ctx context.Context, node *Node,
	params MaterializeParams) (map[uint32]MaterializeParams, error) {

	// Compute child internal keys and asset amounts.
	childKeys, err := ComputeChildInternalKeys(node)
	if err != nil {
		return nil, fmt.Errorf("compute child internal keys: %w", err)
	}

	childAmounts := ComputeChildAssetAmounts(node)

	// Build child output specs.
	indices := sortedChildIndices(node.Children)
	childOutputs := make([]ChildOutputSpec, len(indices))
	for i, idx := range indices {
		childOutputs[i] = ChildOutputSpec{
			AnchorKey:   childKeys[idx],
			AssetAmount: childAmounts[idx],
		}
	}

	// Build output values map.
	outputValues := make(map[uint32]int64, len(indices))
	for i := range indices {
		outputValues[uint32(i)] = params.ChildBtcValue
	}

	// Build the spec for MakeAssetNodeTxBuilder.
	spec := &NodeBuildSpec{
		Input:             params.Input,
		ParentProof:       params.ParentProof,
		IsLeaf:            false,
		ChildOutputs:      childOutputs,
		AssetOutputValues: outputValues,
	}

	// Use InputAnchorKey if parent plan is available (assembly path).
	if params.ParentPlan != nil {
		spec.InputAnchorKey = &params.ParentPlan.AnchorKey
	} else {
		// Rebuild path - compute from cosigners.
		spec.CoSigners = node.CoSigners
	}

	// Build the transaction.
	builder, plan, err := MakeAssetNodeTxBuilder(ctx, m.Config, m.Wallet, spec)
	if err != nil {
		return nil, fmt.Errorf("build branch tx: %w", err)
	}

	// Extract outputs from the anchor PSBT.
	anchorPsbt := builder.AnchorPsbt()
	if anchorPsbt == nil {
		return nil, fmt.Errorf("branch anchor psbt missing")
	}

	anchorTx := anchorPsbt.UnsignedTx
	node.Outputs = make([]*wire.TxOut, len(anchorTx.TxOut))
	for i, out := range anchorTx.TxOut {
		node.Outputs[i] = wire.NewTxOut(out.Value, out.PkScript)
	}

	// Update WitnessUtxo value if needed.
	if len(anchorPsbt.Inputs) > 0 && anchorPsbt.Inputs[0].WitnessUtxo != nil {
		anchorPsbt.Inputs[0].WitnessUtxo.Value = params.InputBtcValue
	}

	// Extract proof suffixes for each child.
	proofs, err := proofsByOutput(builder.ActivePackets(), len(indices))
	if err != nil {
		return nil, fmt.Errorf("branch proof suffix: %w", err)
	}

	// Build plans array.
	plans := make([]*assets.AnchorPlan, len(plan.OutputPlans))
	for i := range plan.OutputPlans {
		plans[i] = &plan.OutputPlans[i]
	}

	// Attach taproot tweak from parent proof.
	if err := attachTaprootTweakFromParent(node, params.ParentPlan,
		params.ParentProof); err != nil {

		return nil, fmt.Errorf("attach branch taproot tweak: %w", err)
	}

	// Get parent TXID for child inputs.
	parentTxHash, err := node.TXID()
	if err != nil {
		return nil, fmt.Errorf("get parent txid: %w", err)
	}

	// Build child params.
	childParams := make(map[uint32]MaterializeParams)
	for i, idx := range indices {
		child := node.Children[idx]

		childParams[idx] = MaterializeParams{
			Input: wire.OutPoint{
				Hash:  parentTxHash,
				Index: idx,
			},
			ParentProof:   proofs[i],
			ParentPlan:    plans[i],
			InputBtcValue: params.ChildBtcValue,
			ChildBtcValue: computeChildBtcValue(child,
				params.ChildBtcValue),
		}
	}

	return childParams, nil
}

// MakeAssetLeafOutputCfg creates the OutputConfig for a leaf VTXO.
// Used by both initial assembly and rebuild paths. Leaf VTXOs use a NUMS
// internal key (no keyspend) with collaborative and timeout script paths.
func MakeAssetLeafOutputCfg(ownerKey, operatorKey *btcec.PublicKey,
	amount uint64, csvDelay uint32) assets.OutputConfig {

	internalKey := &scripts.ARKNUMSKey

	// Collaborative path: owner (client) + cosigner (operator).
	collabClosure := (&assets.CollabMultisigClosure{
		OwnerKey:    ownerKey,
		CosignerKey: operatorKey,
	}).ScriptClosure()

	// Timeout path: owner (client) can sweep after CSV delay expires.
	timeoutClosure := (&assets.VTXOTimeoutClosure{
		Key:   ownerKey,
		Delay: csvDelay,
	}).ScriptClosure()

	return assets.OutputConfig{
		Amount: amount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(internalKey),
		},
		Closures: []assets.ScriptClosure{
			collabClosure, timeoutClosure,
		},
		Script: assets.OpTrueUniqueScript(internalKey),
	}
}

// MakeAssetBranchOutputCfg creates the OutputConfig for a branch child.
// Used by both initial assembly and rebuild paths. Branch outputs use a CSV
// closure with the operator key for sweep capability.
func MakeAssetBranchOutputCfg(internalKey, operatorKey *btcec.PublicKey,
	amount uint64, csvDelay uint32, isSplitRoot bool) assets.OutputConfig {

	csvClosure := (&assets.CSVClosure{
		Key:   operatorKey,
		Delay: csvDelay,
	}).ScriptClosure()

	cfg := assets.OutputConfig{
		Amount: amount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(internalKey),
		},
		Closures: []assets.ScriptClosure{csvClosure},
		Script:   assets.OpTrueUniqueScript(internalKey),
	}

	if isSplitRoot {
		cfg.Type = tappsbt.TypeSplitRoot
	}

	return cfg
}

// MakeAssetBranchInputCfg creates the InputConfig for spending a branch output.
// Branch outputs use a CSV closure with the operator key for sweep capability.
// The internalKey is the MuSig2 aggregate of the node's cosigners, which was
// used as the anchor key when creating the parent's output.
//
// Used by both initial assembly and rebuild paths to ensure explicit and
// consistent input configuration.
func MakeAssetBranchInputCfg(parentProof []byte, internalKey,
	operatorKey *btcec.PublicKey, csvDelay uint32) assets.InputConfig {

	csvClosure := (&assets.CSVClosure{
		Key:   operatorKey,
		Delay: csvDelay,
	}).ScriptClosure()

	return assets.InputConfig{
		ProofFile: parentProof,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(internalKey),
		},
		Closures: []assets.ScriptClosure{csvClosure},
	}
}

// MakeAssetBranchInputCfgWithSpec creates the InputConfig for spending a branch
// output using a pre-computed AnchorKeySpec. This variant is useful when the
// anchor key spec is already available (e.g., from a parent's AnchorPlan).
//
// Used by the assembler where parentPlan.AnchorKey is already available.
func MakeAssetBranchInputCfgWithSpec(parentProof []byte,
	anchorKey assets.AnchorKeySpec, operatorKey *btcec.PublicKey,
	csvDelay uint32) assets.InputConfig {

	csvClosure := (&assets.CSVClosure{
		Key:   operatorKey,
		Delay: csvDelay,
	}).ScriptClosure()

	return assets.InputConfig{
		ProofFile: parentProof,
		AnchorKey: anchorKey,
		Closures:  []assets.ScriptClosure{csvClosure},
	}
}

// MakeAssetNodeTxBuilder constructs an AssetTxBuilder from a NodeBuildSpec.
// This is the unified builder construction function used by both assembly and
// rebuild paths. The spec provides all necessary information to construct the
// transaction without requiring a fully populated Node struct.
//
// The builder is returned after Compile and Commit. The transaction plan from
// Compile is also returned for callers that need it (e.g., the assembly path
// for attaching output metadata via OutputPlans).
//
// The wallet parameter is used to call Commit, which generates the virtual
// PSBTs and proof suffixes via tapd. All tree nodes use zero-fee transactions
// with ephemeral anchors, so SkipWalletFunding and SkipZeroFeeBalance are
// always set.
func MakeAssetNodeTxBuilder(ctx context.Context, cfg AssetTreeConfig,
	wallet assets.AssetWalletClient,
	spec *NodeBuildSpec) (*assets.AssetTxBuilder, *assets.AssetTxPlan, error) {

	if spec == nil {
		return nil, nil, fmt.Errorf("spec is nil")
	}

	if len(spec.ParentProof) == 0 {
		return nil, nil, fmt.Errorf("parent proof required")
	}

	// Either InputAnchorKey or CoSigners must be provided.
	if spec.InputAnchorKey == nil && len(spec.CoSigners) == 0 {
		return nil, nil, fmt.Errorf("no anchor key or cosigners provided")
	}

	builder := assets.NewAssetTxBuilder(cfg.AssetID, cfg.ChainParams)

	// Build the input config. Prefer InputAnchorKey if provided (assembly
	// path), otherwise compute from cosigners (rebuild path).
	var inputCfg assets.InputConfig
	if spec.InputAnchorKey != nil {
		inputCfg = MakeAssetBranchInputCfgWithSpec(
			spec.ParentProof, *spec.InputAnchorKey,
			cfg.OperatorKey, cfg.CSVDelay,
		)
	} else {
		// Compute the internal key from cosigners. This is the anchor
		// key that was used when creating the parent's output for this
		// node to spend.
		internalKey, err := ComputeInternalKey(spec.CoSigners)
		if err != nil {
			return nil, nil, fmt.Errorf("compute internal key: %w",
				err)
		}

		inputCfg = MakeAssetBranchInputCfg(
			spec.ParentProof, internalKey, cfg.OperatorKey,
			cfg.CSVDelay,
		)
	}

	if err := builder.AddAssetInput(inputCfg); err != nil {
		return nil, nil, fmt.Errorf("add input: %w", err)
	}

	// Add outputs based on whether this is a leaf or branch.
	if spec.IsLeaf {
		if spec.LeafOwnerKey == nil {
			return nil, nil, fmt.Errorf("leaf owner key required")
		}

		outCfg := MakeAssetLeafOutputCfg(
			spec.LeafOwnerKey, cfg.OperatorKey,
			spec.LeafAssetAmount, cfg.CSVDelay,
		)
		if err := builder.AddAssetOutput(outCfg); err != nil {
			return nil, nil, fmt.Errorf("add leaf output: %w", err)
		}
	} else {
		if len(spec.ChildOutputs) == 0 {
			return nil, nil, fmt.Errorf("branch requires child outputs")
		}

		for i, child := range spec.ChildOutputs {
			// Prefer explicit AnchorKey if provided (assembly path),
			// otherwise compute from CoSigners (rebuild path).
			var childInternalKey *btcec.PublicKey
			if child.AnchorKey != nil {
				childInternalKey = child.AnchorKey
			} else {
				var err error
				childInternalKey, err = ComputeInternalKey(
					child.CoSigners,
				)
				if err != nil {
					return nil, nil, fmt.Errorf("compute "+
						"child %d internal key: %w",
						i, err)
				}
			}

			// First output in multi-output split is the split root.
			isSplitRoot := len(spec.ChildOutputs) > 1 && i == 0

			outCfg := MakeAssetBranchOutputCfg(
				childInternalKey, cfg.OperatorKey,
				child.AssetAmount, cfg.CSVDelay,
				isSplitRoot,
			)

			if err := builder.AddAssetOutput(outCfg); err != nil {
				return nil, nil, fmt.Errorf("add child %d "+
					"output: %w", i, err)
			}
		}
	}

	// Add ephemeral anchor for 0-fee package submission.
	anchorSpec := assets.NewEphemeralBTCAnchorSpec()
	if spec.IsLeaf {
		anchorSpec.Description = "leaf-ephemeral-anchor"
	} else {
		anchorSpec.Description = "branch-ephemeral-anchor"
	}
	if err := builder.AddBTCAnchor(anchorSpec); err != nil {
		return nil, nil, fmt.Errorf("add anchor: %w", err)
	}

	// Compile the transaction and capture the plan.
	plan, err := builder.Compile(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("compile: %w", err)
	}

	// Set the input outpoint to match the actual transaction.
	if err := builder.SetInputOutpoint(ctx, 0, spec.Input); err != nil {
		return nil, nil, fmt.Errorf("set input outpoint: %w", err)
	}

	// Commit to get virtual PSBTs from tapd. Tree nodes are always zero-fee
	// (broadcast via package relay with ephemeral anchors).
	commitOpts := assets.CommitOptions{
		SkipWalletFunding:  true,
		SkipZeroFeeBalance: true,
		AssetOutputValues:  spec.AssetOutputValues,
	}
	if err := builder.Commit(ctx, wallet, commitOpts); err != nil {
		return nil, nil, fmt.Errorf("commit: %w", err)
	}

	return builder, plan, nil
}

// NodeBuildSpecFromNode creates a NodeBuildSpec from an existing Node. This is
// used by the rebuild/finalize paths to convert stored node data into the
// unified spec format.
func NodeBuildSpecFromNode(n *Node, parentProof []byte,
	operatorKey *btcec.PublicKey) (*NodeBuildSpec, error) {

	if n == nil {
		return nil, fmt.Errorf("node is nil")
	}

	spec := &NodeBuildSpec{
		Input:       n.Input,
		ParentProof: parentProof,
		CoSigners:   n.CoSigners,
		IsLeaf:      len(n.Children) == 0,
	}

	if spec.IsLeaf {
		if n.Metadata == nil || n.Metadata.Leaf == nil {
			return nil, fmt.Errorf("leaf node missing metadata")
		}

		// Find the leaf's cosigner key (the non-operator key).
		spec.LeafOwnerKey = findLeafKey(n.CoSigners, operatorKey)
		if spec.LeafOwnerKey == nil {
			return nil, fmt.Errorf("leaf cosigner key not found")
		}

		spec.LeafAssetAmount = n.Metadata.Leaf.AssetAmount
	} else {
		// Build child output specs from children.
		spec.ChildOutputs = make([]ChildOutputSpec, 0, len(n.Children))

		// Sort children by output index for deterministic ordering.
		indices := make([]uint32, 0, len(n.Children))
		for idx := range n.Children {
			indices = append(indices, idx)
		}
		slices.Sort(indices)

		for _, idx := range indices {
			child := n.Children[idx]
			spec.ChildOutputs = append(spec.ChildOutputs,
				ChildOutputSpec{
					CoSigners:   child.CoSigners,
					AssetAmount: computeSubtreeAssetAmount(child),
				},
			)
		}
	}

	return spec, nil
}

// findLeafKey finds the leaf's cosigner key (the non-operator key) from the
// list of cosigners.
func findLeafKey(cosigners []*btcec.PublicKey,
	operatorKey *btcec.PublicKey) *btcec.PublicKey {

	for _, k := range cosigners {
		if !k.IsEqual(operatorKey) {
			return k
		}
	}

	return nil
}

// computeSubtreeAssetAmount calculates the total asset amount for a subtree
// by summing all leaf amounts.
func computeSubtreeAssetAmount(node *Node) uint64 {
	if node == nil {
		return 0
	}

	// Leaf node - return its amount.
	if len(node.Children) == 0 {
		if node.Metadata != nil && node.Metadata.Leaf != nil {
			return node.Metadata.Leaf.AssetAmount
		}
		return 0
	}

	// Branch node - sum children.
	var total uint64
	for _, child := range node.Children {
		total += computeSubtreeAssetAmount(child)
	}
	return total
}

// FinalizeAssetNode reconstructs the builder using MakeAssetNodeTxBuilder,
// applies the node's signature, and returns the signed anchor transaction
// together with the builder so the caller can broadcast externally and build
// proofs via builder.Proof(). This should be called after tree signing is
// complete.
//
// The parentProof parameter is the serialized proof for the parent's output
// that this node spends. For root nodes this comes from an external source
// (e.g., onboarding proof).
//
// The method:
//  1. Validates the node has a signature
//  2. Reconstructs the builder using MakeAssetNodeTxBuilder
//  3. Commits the builder to get virtual PSBTs
//  4. Applies the MuSig2 signature to create a finalized PSBT
//  5. Returns the signed anchor tx and builder so the caller can broadcast
//     and call builder.Proof() after confirmation
func (m *AssetMaterializer) FinalizeAssetNode(ctx context.Context, n *Node,
	parentProof []byte) (*FinalizeResult, error) {

	// Validate prerequisites.
	if n.Signature == nil {
		return nil, fmt.Errorf("node has no signature")
	}

	if len(parentProof) == 0 {
		return nil, fmt.Errorf("parent proof required for finalization")
	}

	// Derive BTC output values from the node's stored outputs. This ensures
	// the rebuilt transaction has the same BTC values as the original,
	// which is critical for signature validity.
	outputValues := make(map[uint32]int64)
	for i, out := range n.Outputs {
		// Skip the ephemeral anchor output (value 0, P2A script).
		if out.Value > 0 {
			outputValues[uint32(i)] = out.Value
		}
	}

	// Build spec from stored node data.
	spec, err := NodeBuildSpecFromNode(n, parentProof, m.Config.OperatorKey)
	if err != nil {
		return nil, fmt.Errorf("build spec from node: %w", err)
	}
	spec.AssetOutputValues = outputValues

	// Rebuild the builder using the unified construction function.
	builder, _, err := MakeAssetNodeTxBuilder(ctx, m.Config, m.Wallet, spec)
	if err != nil {
		return nil, fmt.Errorf("rebuild builder: %w", err)
	}

	// Get the transfer data containing the PSBTs.
	td, err := builder.GetTransferData()
	if err != nil {
		return nil, fmt.Errorf("get transfer data: %w", err)
	}

	if len(td.AnchorPsbt) == 0 {
		return nil, fmt.Errorf("transfer data has no anchor PSBT")
	}

	if len(td.VirtualPsbts) == 0 {
		return nil, fmt.Errorf("transfer data has no virtual PSBTs")
	}

	// Parse the unsigned anchor PSBT.
	anchorPkt, err := psbt.NewFromRawBytes(
		bytes.NewReader(td.AnchorPsbt), false,
	)
	if err != nil {
		return nil, fmt.Errorf("parse anchor psbt: %w", err)
	}

	// Verify the PSBT has exactly one input (tree nodes spend single input).
	if len(anchorPkt.Inputs) != 1 {
		return nil, fmt.Errorf("expected 1 input, got %d",
			len(anchorPkt.Inputs))
	}

	// Apply the signature using the standard PSBT field. This mirrors
	// AssetTxBuilder.ApplyKeySpendSignature.
	anchorPkt.Inputs[0].TaprootKeySpendSig = n.Signature.Serialize()

	// Finalize the PSBT to convert TaprootKeySpendSig → FinalScriptWitness.
	// This is what wallet.FinalizePsbt would do.
	if err := psbt.MaybeFinalizeAll(anchorPkt); err != nil {
		return nil, fmt.Errorf("finalize psbt: %w", err)
	}

	// Update the builder's anchor PSBT so that builder.Proof() can extract
	// the finalized transaction. Without this, Proof() would fail with
	// "PSBT cannot be extracted as it is incomplete".
	builder.SetAnchorPsbt(anchorPkt)

	// Extract the signed transaction from the finalized PSBT. This is the
	// transaction that should be broadcast, as the proofs will reference
	// this transaction's TXID.
	anchorTx, err := psbt.Extract(anchorPkt)
	if err != nil {
		return nil, fmt.Errorf("extract anchor tx: %w", err)
	}

	// Serialize the finalized PSBT.
	var signedBuf bytes.Buffer
	if err := anchorPkt.Serialize(&signedBuf); err != nil {
		return nil, fmt.Errorf("serialize signed psbt: %w", err)
	}

	// Generate the OP_TRUE witness for the asset layer. Since the asset
	// script is OP_TRUE (anyone can spend via script path), we don't need
	// a signature - just the tapscript witness (script + control block).
	//
	// The witness is derived from the parent proof's internal key, which
	// determines whether we use standard OP_TRUE (NUMS) or unique OP_TRUE
	// (anchor internal key).
	opTrueWitness, err := deriveOpTrueWitness(parentProof)
	if err != nil {
		return nil, fmt.Errorf("derive OP_TRUE witness: %w", err)
	}

	signedVirtualPsbts := make([][]byte, len(td.VirtualPsbts))
	for i, vPsbtBytes := range td.VirtualPsbts {
		vPkt, decodeErr := tappsbt.Decode(vPsbtBytes)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode virtual psbt %d: %w",
				i, decodeErr)
		}

		// Update each output asset's witness. For split commitments, we
		// need to update the RootAsset's witness directly because
		// Asset.UpdateTxWitness copies RootAsset by value.
		//
		// TODO(PR #1897): Once taproot-assets PR #1897 is merged and
		// dependency bumped, remove this manual root-witness patch and
		// rely on UpdateTxWitness.
		for _, out := range vPkt.Outputs {
			if out.Asset == nil {
				continue
			}

			if len(out.Asset.PrevWitnesses) == 0 {
				continue
			}

			if out.Asset.HasSplitCommitmentWitness() {
				splitCommit := out.Asset.PrevWitnesses[0].
					SplitCommitment
				if splitCommit != nil &&
					len(splitCommit.RootAsset.
						PrevWitnesses) > 0 {

					splitCommit.RootAsset.
						PrevWitnesses[0].
						TxWitness = opTrueWitness
				}
			} else {
				out.Asset.PrevWitnesses[0].
					TxWitness = opTrueWitness
			}
		}

		// Re-encode the virtual PSBT with the witness.
		signedVPsbt, encodeErr := tappsbt.Encode(vPkt)
		if encodeErr != nil {
			return nil, fmt.Errorf("encode virtual psbt %d: %w",
				i, encodeErr)
		}
		signedVirtualPsbts[i] = signedVPsbt
	}

	return &FinalizeResult{
		AnchorTx: anchorTx,
		Builder:  builder,
		TransferData: &assets.TransferData{
			AnchorPsbt:        signedBuf.Bytes(),
			VirtualPsbts:      signedVirtualPsbts,
			PassivePsbts:      td.PassivePsbts,
			ChangeOutputIndex: td.ChangeOutputIndex,
		},
	}, nil
}

// deriveOpTrueWitness derives the OP_TRUE tapscript witness from the parent
// proof. The witness format is [tapScript, controlBlockBytes].
//
// The function checks whether the parent output uses standard NUMS-based
// OP_TRUE or unique OP_TRUE (with custom internal key). This is determined by
// comparing the script key in the proof against the NUMS OP_TRUE output key.
//
// For unique OP_TRUE, the internal key is derived from:
// 1. TweakedScriptKey.RawKey.PubKey if available (preferred)
// 2. InclusionProof.InternalKey as fallback
func deriveOpTrueWitness(parentProof []byte) (wire.TxWitness, error) {
	// Decode the proof file to access the last proof's details.
	proofFile, err := proof.DecodeFile(parentProof)
	if err != nil {
		return nil, fmt.Errorf("decode proof file: %w", err)
	}

	lastProof, err := proofFile.LastProof()
	if err != nil {
		return nil, fmt.Errorf("get last proof: %w", err)
	}

	// Get the input's script key from the proof. This is the tweaked output
	// key that was created when the parent output was formed.
	inputScriptKey := lastProof.Asset.ScriptKey.PubKey
	if inputScriptKey == nil {
		return nil, fmt.Errorf("proof asset has no script key")
	}

	// Check if this is a standard NUMS-based OP_TRUE output. These use the
	// NUMS internal key and produce a deterministic output key.
	numsArtifacts, err := assets.BuildOpTrueArtifacts()
	if err != nil {
		return nil, fmt.Errorf("build NUMS OP_TRUE artifacts: %w", err)
	}

	if numsArtifacts.OutputKey.IsEqual(inputScriptKey) {
		// Standard NUMS-based OP_TRUE - use the NUMS witness.
		return numsArtifacts.Witness, nil
	}

	// Not NUMS-based, so this uses a unique OP_TRUE with a custom internal
	// key. Try to extract the internal key from TweakedScriptKey first
	// (preferred), then fall back to InclusionProof.InternalKey.
	//
	// TweakedScriptKey.RawKey contains the untweaked internal key used to
	// construct the script key. For OP_TRUE outputs, this is the key passed
	// to BuildOpTrueArtifactsWithKey when the output was created.
	var internalKey *btcec.PublicKey
	if lastProof.Asset.ScriptKey.TweakedScriptKey != nil &&
		lastProof.Asset.ScriptKey.TweakedScriptKey.RawKey.PubKey != nil {

		internalKey = lastProof.Asset.ScriptKey.TweakedScriptKey.
			RawKey.PubKey
	} else if lastProof.InclusionProof.InternalKey != nil {
		// Fallback to inclusion proof internal key.
		internalKey = lastProof.InclusionProof.InternalKey
	}

	if internalKey == nil {
		return nil, fmt.Errorf("proof missing internal key for " +
			"unique OP_TRUE")
	}

	// Build OP_TRUE artifacts with the internal key.
	uniqueArtifacts, err := assets.BuildOpTrueArtifactsWithKey(internalKey)
	if err != nil {
		return nil, fmt.Errorf("build unique OP_TRUE artifacts: %w",
			err)
	}

	// Debug: verify the computed output key matches the script key in the
	// proof. If they don't match, the witness will fail verification.
	if !uniqueArtifacts.OutputKey.IsEqual(inputScriptKey) {
		return nil, fmt.Errorf("internal key mismatch: computed "+
			"output key %x does not match proof script key %x "+
			"(internal key: %x, inclusion proof internal key: %x)",
			schnorr.SerializePubKey(uniqueArtifacts.OutputKey),
			schnorr.SerializePubKey(inputScriptKey),
			schnorr.SerializePubKey(internalKey),
			schnorr.SerializePubKey(
				lastProof.InclusionProof.InternalKey,
			))
	}

	return uniqueArtifacts.Witness, nil
}
