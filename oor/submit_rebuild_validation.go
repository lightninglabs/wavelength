package oor

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo/vtxo"
)

// SubmitOutputPolicy defines optional server-side constraints for Ark outputs.
type SubmitOutputPolicy struct {
	MinVTXOAmount btcutil.Amount
	MaxVTXOAmount btcutil.Amount
	DustAmount    btcutil.Amount
}

// validateSubmitRebuildAndPolicy reconstructs all checkpoint and Ark
// transactions from authoritative store data and enforces output policy.
//
// Validation rules:
//   - Every Ark input must map to a known, spendable VTXO in the store.
//   - Each checkpoint txid must match a rebuilt checkpoint derived from the
//     operator policy and the owner leaf in the Ark tap tree.
//   - The Ark txid must match a rebuilt Ark transaction from the derived
//     checkpoint outputs and the Ark recipient set.
//   - Optional output policy constraints (min/max/dust, anchor/op_return) are
//     enforced on the Ark outputs.
//
// This function intentionally does not mutate any state; it is pure validation
// based on the caller-provided store and policy.
func validateSubmitRebuildAndPolicy(ctx context.Context, ark *psbt.Packet,
	checkpoints []*psbt.Packet, descs []VTXOSigningDescriptor,
	checkpointPolicy arkscript.CheckpointPolicy, store vtxo.Store,
	policy SubmitOutputPolicy) error {

	if ark == nil || ark.UnsignedTx == nil {
		return fmt.Errorf("ark psbt must be provided")
	}

	if checkpointPolicy.OperatorKey == nil {
		return fmt.Errorf("checkpoint operator key must be provided")
	}

	if store == nil {
		return fmt.Errorf("vtxo store must be provided")
	}

	descByOutpoint := make(
		map[wire.OutPoint]VTXOSigningDescriptor, len(descs),
	)
	for _, desc := range descs {
		descByOutpoint[desc.Outpoint] = desc
	}

	// Index checkpoints by txid for lookup and verify single-output shape.
	checkpointByTxid := make(
		map[wire.OutPoint]*psbt.Packet, len(checkpoints),
	)
	for _, cp := range checkpoints {
		if cp == nil || cp.UnsignedTx == nil {
			return fmt.Errorf("checkpoint psbt must include " +
				"unsigned tx")
		}

		if len(cp.UnsignedTx.TxOut) == 0 {
			return fmt.Errorf("checkpoint tx has no outputs")
		}

		outpoint := wire.OutPoint{
			Hash:  cp.UnsignedTx.TxHash(),
			Index: 0,
		}
		checkpointByTxid[outpoint] = cp
	}

	// Rebuild checkpoints and ark tx from authoritative input data.
	checkpointOuts := make(
		[]oorlib.CheckpointOutput, 0, len(ark.UnsignedTx.TxIn),
	)
	txContextByCheckpoint := make(
		map[wire.OutPoint]rebuildTxContext, len(ark.UnsignedTx.TxIn),
	)
	for i, txIn := range ark.UnsignedTx.TxIn {
		checkpointOut, txContext, err := rebuildCheckpointOutput(
			ctx, ark, i, txIn.PreviousOutPoint, checkpointByTxid,
			descByOutpoint, checkpointPolicy, store,
		)
		if err != nil {
			return err
		}

		checkpointOuts = append(checkpointOuts, checkpointOut)
		txContextByCheckpoint[wire.OutPoint{
			Hash:  checkpointOut.Txid,
			Index: 0,
		}] = txContext
	}

	recipients, err := extractArkRecipientsForRebuild(ark)
	if err != nil {
		return fmt.Errorf("extract ark recipients: %w", err)
	}

	if err := validateArkOutputs(recipients, policy, ark); err != nil {
		return err
	}

	rebuiltArk, err := oorlib.BuildArkPSBT(checkpointOuts, recipients)
	if err != nil {
		return fmt.Errorf("rebuild ark psbt: %w", err)
	}

	if err := applyArkRebuildTxContexts(
		rebuiltArk, txContextByCheckpoint,
	); err != nil {
		return err
	}

	if rebuiltArk.UnsignedTx.TxHash() != ark.UnsignedTx.TxHash() {
		return fmt.Errorf("ark txid mismatch")
	}

	return nil
}

// rebuildCheckpointOutput reconstructs one checkpoint from authoritative VTXO
// state and validates that it matches the submitted checkpoint transaction.
func rebuildCheckpointOutput(ctx context.Context, ark *psbt.Packet,
	inputIndex int, checkpointOutpoint wire.OutPoint,
	checkpointByTxid map[wire.OutPoint]*psbt.Packet,
	descByOutpoint map[wire.OutPoint]VTXOSigningDescriptor,
	checkpointPolicy arkscript.CheckpointPolicy, store vtxo.Store) (
	oorlib.CheckpointOutput, rebuildTxContext, error) {

	var (
		emptyOutput  oorlib.CheckpointOutput
		emptyContext rebuildTxContext
	)

	cp := checkpointByTxid[checkpointOutpoint]
	if cp == nil {
		return emptyOutput, emptyContext, fmt.Errorf("ark input %d "+
			"references unknown checkpoint", inputIndex)
	}

	if len(cp.UnsignedTx.TxIn) != 1 {
		return emptyOutput, emptyContext, fmt.Errorf("checkpoint tx " +
			"must have exactly one input")
	}

	descOutpoint := cp.UnsignedTx.TxIn[0].PreviousOutPoint
	desc, ok := descByOutpoint[descOutpoint]
	if !ok {
		return emptyOutput, emptyContext, fmt.Errorf("missing signing "+
			"descriptor for %s", descOutpoint)
	}

	rec, spendPath, err := validateRebuildRecord(ctx, store, desc)
	if err != nil {
		return emptyOutput, emptyContext, err
	}

	if len(cp.Inputs) == 0 || cp.Inputs[0].WitnessUtxo == nil {
		return emptyOutput, emptyContext, fmt.Errorf("checkpoint " +
			"missing witness utxo")
	}

	cpUtxo := cp.Inputs[0].WitnessUtxo
	if !bytes.Equal(rec.PkScript, cpUtxo.PkScript) {
		return emptyOutput, emptyContext, fmt.Errorf("checkpoint " +
			"input pkscript mismatch")
	}

	if rec.Value != cpUtxo.Value {
		return emptyOutput, emptyContext, fmt.Errorf("vtxo amount " +
			"mismatch")
	}

	ownerLeaf, err := findOwnerLeafScript(
		ark, inputIndex, cp, desc, checkpointPolicy,
	)
	if err != nil {
		return emptyOutput, emptyContext, err
	}

	artifact, err := oorlib.BuildCheckpointPSBT(
		checkpointPolicy,
		oorlib.CheckpointInput{
			SpentVTXO: oorlib.SpentVTXORef{
				Outpoint: desc.Outpoint,
				Output: &wire.TxOut{
					Value:    rec.Value,
					PkScript: rec.PkScript,
				},
			},
			OwnerLeafScript: ownerLeaf,
		},
	)
	if err != nil {
		return emptyOutput, emptyContext, err
	}

	txContext := applySpendPathTxContext(artifact.PSBT, spendPath)

	checkpointOut, err := artifact.ToCheckpointOutput()
	if err != nil {
		return emptyOutput, emptyContext, err
	}

	if checkpointOut.Txid != cp.UnsignedTx.TxHash() {
		return emptyOutput, emptyContext, fmt.Errorf("checkpoint " +
			"txid mismatch")
	}

	return checkpointOut, txContext, nil
}

// rebuildTxContext is the transaction-level context required by one selected
// VTXO spend path.
type rebuildTxContext struct {
	sequence uint32
	lockTime uint32
}

// applySpendPathTxContext mirrors spend-path locktime and sequence constraints
// onto the transaction that spends the selected leaf. The returned context is
// propagated to the Ark rebuild because the Ark transaction spends the rebuilt
// checkpoint output.
func applySpendPathTxContext(pkt *psbt.Packet,
	spendPath *arkscript.SpendPath) rebuildTxContext {

	txContext := txContextForSpendPath(spendPath)
	if pkt == nil || pkt.UnsignedTx == nil {
		return txContext
	}

	if txContext.lockTime != 0 {
		pkt.UnsignedTx.LockTime = txContext.lockTime
	}

	for i := range pkt.UnsignedTx.TxIn {
		pkt.UnsignedTx.TxIn[i].Sequence = txContext.sequence
	}

	return txContext
}

// applyArkRebuildTxContexts applies each rebuilt checkpoint's spend context to
// the Ark transaction that spends those checkpoints.
func applyArkRebuildTxContexts(pkt *psbt.Packet,
	contexts map[wire.OutPoint]rebuildTxContext) error {

	if pkt == nil || pkt.UnsignedTx == nil {
		return fmt.Errorf("ark psbt must be provided")
	}

	for i := range pkt.UnsignedTx.TxIn {
		prevOut := pkt.UnsignedTx.TxIn[i].PreviousOutPoint
		txContext, ok := contexts[prevOut]
		if !ok {
			return fmt.Errorf("missing tx context for ark input "+
				"%d (%s)", i, prevOut)
		}

		if txContext.lockTime > pkt.UnsignedTx.LockTime {
			pkt.UnsignedTx.LockTime = txContext.lockTime
		}
		pkt.UnsignedTx.TxIn[i].Sequence = txContext.sequence
	}

	return nil
}

// txContextForSpendPath returns the tx input sequence and locktime needed for
// one spend path. CLTV leaves require a non-final sequence even when the path
// does not also carry an explicit relative locktime.
func txContextForSpendPath(spendPath *arkscript.SpendPath) rebuildTxContext {
	if spendPath == nil {
		return rebuildTxContext{sequence: wire.MaxTxInSequenceNum}
	}

	txContext := rebuildTxContext{
		sequence: wire.MaxTxInSequenceNum,
		lockTime: spendPath.RequiredLockTime,
	}

	switch {
	case spendPath.RequiredSequence != 0:
		txContext.sequence = spendPath.RequiredSequence

	case spendPath.RequiredLockTime != 0:
		txContext.sequence = wire.MaxTxInSequenceNum - 1
	}

	return txContext
}

// validateRebuildRecord loads and validates the VTXO record for one
// descriptor used during submit rebuild validation.
func validateRebuildRecord(ctx context.Context, store vtxo.Store,
	desc VTXOSigningDescriptor) (*vtxo.Record, *arkscript.SpendPath,
	error) {

	rec, err := store.Get(ctx, desc.Outpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("get vtxo %s: %w", desc.Outpoint,
			err)
	}
	if rec == nil {
		return nil, nil, fmt.Errorf("vtxo %s not found", desc.Outpoint)
	}

	isSpendable := rec.Status == vtxo.StatusLive ||
		rec.Status == vtxo.StatusInFlight
	if !isSpendable {
		return nil, nil, fmt.Errorf("vtxo %s not spendable",
			desc.Outpoint)
	}

	template, err := decodeDescriptorPolicyTemplate(desc)
	if err != nil {
		return nil, nil, err
	}

	spendPath, err := decodeDescriptorSpendPath(desc)
	if err != nil {
		return nil, nil, err
	}

	err = validateSpendPathAgainstPolicy(template, spendPath)
	if err != nil {
		return nil, nil, err
	}

	expectedPk, err := template.PkScript()
	if err != nil {
		return nil, nil, fmt.Errorf("compile vtxo pkscript: %w", err)
	}

	if !bytes.Equal(rec.PkScript, expectedPk) {
		return nil, nil, fmt.Errorf("vtxo pkscript mismatch")
	}

	if len(rec.PolicyTemplate) > 0 && !bytes.Equal(
		rec.PolicyTemplate, desc.VTXOPolicyTemplate,
	) {
		return nil, nil, fmt.Errorf("vtxo policy template mismatch")
	}

	return rec, spendPath, nil
}

// findOwnerLeafScript extracts the owner leaf script for a specific checkpoint
// input from the explicit descriptor policy and verifies it is bound into the
// corresponding checkpoint output tree and Ark spend path.
//
// The checkpoint tap tree for v0 is a two-leaf tree:
//   - operator-controlled CSV unroll leaf (derived from policy)
//   - owner collaborative leaf (selected by the client)
//
// This function ensures the Ark input carries the owner leaf in its PSBT
// metadata so rebuild validation can be deterministic.
func findOwnerLeafScript(ark *psbt.Packet, inputIndex int,
	checkpoint *psbt.Packet, desc VTXOSigningDescriptor,
	policy arkscript.CheckpointPolicy) ([]byte, error) {

	if inputIndex < 0 || inputIndex >= len(ark.Inputs) {
		return nil, fmt.Errorf("ark input %d out of range", inputIndex)
	}
	if checkpoint == nil {
		return nil, fmt.Errorf("checkpoint psbt must be provided")
	}
	if policy.OperatorKey == nil {
		return nil, fmt.Errorf("checkpoint operator key must be " +
			"provided")
	}
	if len(checkpoint.Outputs) == 0 {
		return nil, fmt.Errorf("checkpoint psbt has no outputs")
	}
	if len(checkpoint.Outputs[0].TaprootTapTree) == 0 {
		return nil, fmt.Errorf("checkpoint output tap tree not found")
	}
	if len(desc.OwnerLeafPolicy) == 0 {
		return nil, fmt.Errorf("owner leaf policy not found")
	}

	leaf, err := arkscript.DecodeLeafTemplate(desc.OwnerLeafPolicy)
	if err != nil {
		return nil, err
	}

	if !arkscript.ContainsKey(leaf.Node, policy.OperatorKey) {
		return nil, fmt.Errorf("owner leaf policy does not contain " +
			"operator key")
	}

	ownerLeaf, err := leaf.Script()
	if err != nil {
		return nil, err
	}

	return validateOwnerLeafBinding(
		ark, inputIndex, checkpoint.Outputs[0].TaprootTapTree,
		ownerLeaf, policy,
	)
}

// validateOwnerLeafBinding verifies that the compiled owner leaf is present in
// the encoded checkpoint tree and that the Ark input binds to it directly.
func validateOwnerLeafBinding(ark *psbt.Packet, inputIndex int, encoded []byte,
	ownerLeaf []byte, policy arkscript.CheckpointPolicy) ([]byte, error) {

	leaves, err := oorlib.DecodeTapTree(encoded)
	if err != nil {
		return nil, err
	}

	unrollLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		policy.OperatorKey, policy.CSVDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("unroll leaf: %w", err)
	}

	var (
		foundOwner  bool
		foundUnroll bool
	)
	for _, leaf := range leaves {
		if bytes.Equal(leaf, unrollLeaf.Script) {
			foundUnroll = true
			continue
		}

		if bytes.Equal(leaf, ownerLeaf) {
			foundOwner = true
		}
	}

	if !foundUnroll {
		return nil, fmt.Errorf("unroll leaf not found")
	}
	if !foundOwner {
		return nil, fmt.Errorf("owner leaf not found")
	}

	if len(ark.Inputs[inputIndex].TaprootLeafScript) == 0 {
		return nil, fmt.Errorf("missing ark tapleaf script")
	}

	for _, leaf := range ark.Inputs[inputIndex].TaprootLeafScript {
		if leaf == nil {
			continue
		}
		if bytes.Equal(leaf.Script, ownerLeaf) {
			return ownerLeaf, nil
		}
	}

	return nil, fmt.Errorf("ark input does not use owner leaf")
}

// validateArkOutputs enforces optional output constraints and canonical v0
// output structure (single anchor output, optional op_return) on the Ark tx.
func validateArkOutputs(outputs []oorlib.RecipientOutput,
	policy SubmitOutputPolicy, ark *psbt.Packet) error {

	if ark == nil || ark.UnsignedTx == nil {
		return fmt.Errorf("ark psbt must be provided")
	}

	if policy.DustAmount > 0 {
		for i := range outputs {
			if outputs[i].Value < policy.DustAmount {
				return fmt.Errorf("output %d below dust", i)
			}
		}
	}

	if policy.MinVTXOAmount > 0 {
		for i := range outputs {
			if outputs[i].Value < policy.MinVTXOAmount {
				return fmt.Errorf("output %d below min", i)
			}
		}
	}

	if policy.MaxVTXOAmount > 0 {
		for i := range outputs {
			if outputs[i].Value > policy.MaxVTXOAmount {
				return fmt.Errorf("output %d above max", i)
			}
		}
	}

	foundAnchor := false
	foundOpReturn := false
	anchorPkScript := arkscript.AnchorOutput().PkScript
	for _, out := range ark.UnsignedTx.TxOut {
		if bytes.Equal(out.PkScript, anchorPkScript) {
			if foundAnchor {
				return fmt.Errorf("multiple anchor outputs")
			}
			foundAnchor = true

			continue
		}

		isOpReturn := len(out.PkScript) > 0 &&
			out.PkScript[0] == txscript.OP_RETURN
		if isOpReturn {
			if foundOpReturn {
				return fmt.Errorf("multiple op_return outputs")
			}
			foundOpReturn = true
		}
	}

	if !foundAnchor {
		return fmt.Errorf("missing anchor output")
	}

	return nil
}

// extractArkRecipientsForRebuild adapts client-side recipient extraction to
// the server-side rebuild validator output type.
func extractArkRecipientsForRebuild(ark *psbt.Packet) ([]oorlib.RecipientOutput,
	error) {

	recipients, err := clientoor.ExtractArkRecipients(ark)
	if err != nil {
		return nil, err
	}

	out := make([]oorlib.RecipientOutput, 0, len(recipients))
	for _, recipient := range recipients {
		out = append(out, oorlib.RecipientOutput{
			PkScript: recipient.PkScript,
			Value:    recipient.Value,
		})
	}

	return out, nil
}
