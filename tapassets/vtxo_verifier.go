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
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
)

// ProofChainVerifier verifies a confirmed proof file against the chain: the
// block inclusion of every anchor and every asset transition back to issuance.
// It is the only part of asset VTXO verification that needs chain data, so it
// is the part an asset backend supplies.
type ProofChainVerifier interface {
	// VerifyProofChain returns an error unless the proof file is valid.
	VerifyProofChain(ctx context.Context, proofFile []byte) error
}

// VTXOVerifier checks an asset VTXO before its owner signs the tree path. The
// leaf's sealed package must move exactly the requested asset into the VTXO
// output, and every transition between the confirmed deposits and the leaf
// must be a transaction of the round.
type VTXOVerifier struct {
	chain ProofChainVerifier
}

// NewVTXOVerifier returns a verifier that checks confirmed proofs with chain.
func NewVTXOVerifier(chain ProofChainVerifier) (*VTXOVerifier, error) {
	if chain == nil {
		return nil, fmt.Errorf("proof chain verifier is required")
	}

	return &VTXOVerifier{chain: chain}, nil
}

// VerifyAssetVTXO verifies that the single leaf of clientTree holds
// assetAmount units of assetRef with a valid proof path. The client tree must
// already be bound to commitmentTx.
func (v *VTXOVerifier) VerifyAssetVTXO(ctx context.Context, assetRef string,
	assetAmount uint64, commitmentTx *wire.MsgTx, clientTree *tree.Tree,
	sealedPackage []byte) error {

	if v == nil || v.chain == nil {
		return fmt.Errorf("proof chain verifier is required")
	}
	if commitmentTx == nil || clientTree == nil ||
		clientTree.Root == nil || clientTree.AssetContext == nil {
		return fmt.Errorf("asset VTXO requires a commitment " +
			"transaction and an asset tree path")
	}
	ref, err := tapsdk.ParseAssetRef(assetRef)
	if err != nil {
		return fmt.Errorf("asset VTXO reference: %w", err)
	}

	leaves := clientTree.Root.GetLeafNodes()
	if len(leaves) != 1 {
		return fmt.Errorf("expected one asset VTXO leaf, got %d",
			len(leaves))
	}
	leaf := leaves[0]
	leafOutpoint, err := leaf.GetNonAnchorOutpoint()
	if err != nil {
		return err
	}

	committed, err := decodeSealedPackage(sealedPackage)
	if err != nil {
		return err
	}
	if err := checkPackageTx(committed, leaf); err != nil {
		return err
	}
	var leafRoot tapsdk.Hash
	copy(leafRoot[:], clientTree.AssetContext.LeafAssetRoot(leaf.Input))
	err = checkLeafOutputs(
		committed, ref, assetAmount, sdkOutpoint(*leafOutpoint),
		leafRoot,
	)
	if err != nil {
		return err
	}

	verifier, err := newRoundProofVerifier(
		v.chain, commitmentTx, clientTree, *leafOutpoint,
	)
	if err != nil {
		return err
	}
	for idx := range committed.outputs {
		err := verifyLeafOutputProof(
			ctx, committed, &committed.outputs[idx], ref, verifier,
		)
		if err != nil {
			return fmt.Errorf("asset VTXO output %d: %w", idx, err)
		}
	}

	return nil
}

// checkPackageTx checks that the package seals the leaf's own transaction.
func checkPackageTx(committed *commitResult, leaf *tree.Node) error {
	anchor, err := psbtutil.Parse(committed.anchorPSBT)
	if err != nil {
		return fmt.Errorf("decode sealed leaf transaction: %w", err)
	}
	leafTx, err := leaf.ToTx()
	if err != nil {
		return fmt.Errorf("build asset VTXO leaf transaction: %w", err)
	}
	if anchor.UnsignedTx.TxHash() != leafTx.TxHash() {
		return fmt.Errorf("sealed package does not seal the asset " +
			"VTXO leaf transaction")
	}

	return nil
}

// checkLeafOutputs checks that every asset output of the leaf transaction
// lands in the VTXO, holds the requested asset under an OP_TRUE script key,
// and commits to the leaf's asset root. An OP_TRUE key leaves the VTXO policy
// as the only spending condition. The script mode alone proves the key:
// decoding the package runs tap-sdk's validation, which requires the OP_TRUE
// control block to commit its leaf to the output's script key.
func checkLeafOutputs(committed *commitResult, ref tapsdk.AssetRef,
	assetAmount uint64, leafOutpoint tapsdk.Outpoint,
	leafRoot tapsdk.Hash) error {

	if len(committed.outputs) == 0 {
		return fmt.Errorf("sealed package has no asset outputs")
	}

	var total uint64
	for idx := range committed.outputs {
		output := &committed.outputs[idx]
		switch {
		case output.anchorOutpoint != leafOutpoint:
			return fmt.Errorf("asset output %d is not in the VTXO",
				idx)

		case !output.assetRef.Equivalent(ref):
			return fmt.Errorf("asset output %d holds %s, want %s",
				idx, output.assetRef, ref)

		case output.scriptMode != customanchor.AssetScriptOPTrue ||
			len(output.opTrueWitness) == 0:
			return fmt.Errorf("asset output %d is not "+
				"OP_TRUE locked", idx)

		case output.taprootAssetRoot != leafRoot:
			return fmt.Errorf("asset output %d does not commit to "+
				"the leaf asset root", idx)

		case output.amount > math.MaxUint64-total:
			return fmt.Errorf("asset output amount overflow")
		}
		total += output.amount
	}
	if total != assetAmount {
		return fmt.Errorf("asset VTXO holds %d units, want %d", total,
			assetAmount)
	}

	return nil
}

// verifyLeafOutputProof extends the proof path of the output's input with the
// leaf transition and verifies it back to the confirmed deposits.
func verifyLeafOutputProof(ctx context.Context, committed *commitResult,
	output *commitOutput, ref tapsdk.AssetRef,
	verifier *roundProofVerifier) error {

	var input *commitInput
	for idx := range committed.inputs {
		candidate := &committed.inputs[idx]
		if candidate.packetRole != output.packetRole ||
			candidate.packetIndex != output.packetIndex {

			continue
		}
		if input != nil {
			return fmt.Errorf("leaf transition merges asset inputs")
		}
		input = candidate
	}
	if input == nil {
		return fmt.Errorf("leaf transition has no asset input")
	}

	path, err := inputProofPath(input.proofSource)
	if err != nil {
		return err
	}
	path.Steps = append(path.Steps, customanchor.ProofPathStep{
		TransitionProof: bytes.Clone(output.proofBlob),
	})

	summary, err := path.Verify(ctx, verifier)
	if err != nil {
		return fmt.Errorf("verify asset proof path: %w", err)
	}

	return checkProvenOutput(summary, output, ref)
}

// checkProvenOutput checks that the asset a verified proof path ends in is
// the package output: the same asset and tranche, amount, VTXO and script key.
func checkProvenOutput(summary *customanchor.ProofPathSummary,
	output *commitOutput, ref tapsdk.AssetRef) error {

	switch {
	case summary == nil:
		return fmt.Errorf("proof path has no summary")

	case !summary.AssetRef.Equivalent(ref),
		summary.IssuanceID != output.issuanceID:
		return fmt.Errorf("proven asset does not match the package")

	case summary.Amount != output.amount:
		return fmt.Errorf("proven amount %d does not match the "+
			"package amount %d", summary.Amount, output.amount)

	case summary.AnchorOutpoint != output.anchorOutpoint:
		return fmt.Errorf("proven asset is not in the VTXO")

	case summary.ScriptKey != output.scriptKey:
		return fmt.Errorf("proven script key is not the OP_TRUE key")
	}

	return nil
}

// inputProofPath decodes the proof source of a leaf transition's input.
func inputProofPath(source commitProofSource) (*customanchor.ProofPath, error) {
	switch source.kind {
	case customanchor.ProofSourceCompactPath:
		var path customanchor.ProofPath
		if err := path.UnmarshalBinary(source.blob); err != nil {
			return nil, fmt.Errorf("decode input proof path: %w",
				err)
		}

		return &path, nil

	case customanchor.ProofSourceConfirmedFile:
		return &customanchor.ProofPath{
			ConfirmedBaseProof: bytes.Clone(source.blob),
		}, nil

	default:
		return nil, fmt.Errorf("unknown input proof source kind %d",
			source.kind)
	}
}

// roundProofVerifier verifies the confirmed deposits of an asset VTXO's proof
// path and binds every unconfirmed step to a transaction of the round: the
// commitment transaction or a transaction on the VTXO's tree path.
type roundProofVerifier struct {
	chain        ProofChainVerifier
	transactions map[chainhash.Hash]struct{}
	anchors      map[tapsdk.Outpoint]struct{}
}

// newRoundProofVerifier collects the round transactions and asset anchors
// that an asset VTXO's proof path may pass through.
func newRoundProofVerifier(chain ProofChainVerifier, commitmentTx *wire.MsgTx,
	clientTree *tree.Tree,
	leafOutpoint wire.OutPoint) (*roundProofVerifier, error) {

	verifier := &roundProofVerifier{
		chain: chain,
		transactions: map[chainhash.Hash]struct{}{
			commitmentTx.TxHash(): {},
		},
		anchors: map[tapsdk.Outpoint]struct{}{
			sdkOutpoint(leafOutpoint): {},
		},
	}

	// Each node spends the asset anchor its parent created, and the root
	// spends the commitment transaction's batch output.
	for node := range clientTree.Root.NodesIter() {
		tx, err := node.ToTx()
		if err != nil {
			return nil, fmt.Errorf("build asset tree "+
				"transaction: %w", err)
		}
		verifier.transactions[tx.TxHash()] = struct{}{}
		verifier.anchors[sdkOutpoint(node.Input)] = struct{}{}
	}

	return verifier, nil
}

// VerifyConfirmedProof verifies a deposit's proof chain and derives its anchor
// inventory from the proof, since the deposits of other clients are not in
// this wallet.
func (v *roundProofVerifier) VerifyConfirmedProof(ctx context.Context,
	proofFile []byte) (*customanchor.ConfirmedProofVerification, error) {

	if err := v.chain.VerifyProofChain(ctx, proofFile); err != nil {
		return nil, err
	}

	return customanchor.DeriveConfirmedProofInventory(proofFile)
}

// VerifyUnconfirmedAnchor checks that an unconfirmed step anchors its asset on
// the VTXO's path through the round.
func (v *roundProofVerifier) VerifyUnconfirmedAnchor(_ context.Context,
	transition customanchor.UnconfirmedAnchorVerification) error {

	var tx wire.MsgTx
	err := tx.Deserialize(bytes.NewReader(transition.AnchorTransaction))
	if err != nil {
		return fmt.Errorf("decode step %d transaction: %w",
			transition.StepIndex, err)
	}
	txid := tx.TxHash()
	if _, ok := v.transactions[txid]; !ok {
		return fmt.Errorf("step %d transaction %v is not in the round",
			transition.StepIndex, txid)
	}
	if chainhash.Hash(transition.AnchorOutpoint.Txid) != txid {
		return fmt.Errorf("step %d anchor is not in its transaction",
			transition.StepIndex)
	}
	if _, ok := v.anchors[transition.AnchorOutpoint]; !ok {
		return fmt.Errorf("step %d anchor is not on the VTXO path",
			transition.StepIndex)
	}

	return nil
}
