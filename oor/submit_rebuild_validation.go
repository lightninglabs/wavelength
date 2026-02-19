package oor

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
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
	checkpointPolicy scripts.CheckpointPolicy, store vtxo.Store,
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
	for i, txIn := range ark.UnsignedTx.TxIn {
		prevOut := txIn.PreviousOutPoint
		cp := checkpointByTxid[prevOut]
		if cp == nil {
			return fmt.Errorf("ark input %d references unknown "+
				"checkpoint", i)
		}

		if len(cp.UnsignedTx.TxIn) != 1 {
			return fmt.Errorf("checkpoint tx must have exactly " +
				"one input")
		}

		descOutpoint := cp.UnsignedTx.TxIn[0].PreviousOutPoint
		desc, ok := descByOutpoint[descOutpoint]
		if !ok {
			return fmt.Errorf("missing signing descriptor for %s",
				cp.UnsignedTx.TxIn[0].PreviousOutPoint)
		}

		rec, err := store.Get(ctx, desc.Outpoint)
		if err != nil {
			return fmt.Errorf("get vtxo %s: %w", desc.Outpoint, err)
		}
		if rec == nil {
			return fmt.Errorf("vtxo %s not found", desc.Outpoint)
		}
		isSpendable := rec.Status == vtxo.StatusLive ||
			rec.Status == vtxo.StatusInFlight
		if !isSpendable {
			return fmt.Errorf("vtxo %s not spendable",
				desc.Outpoint)
		}

		tapKey, err := scripts.VTXOTapKey(
			desc.OwnerKey, checkpointPolicy.OperatorKey,
			desc.ExitDelay,
		)
		if err != nil {
			return fmt.Errorf("vtxo tapscript: %w", err)
		}

		expectedPk, err := txscript.PayToTaprootScript(tapKey)
		if err != nil {
			return fmt.Errorf("vtxo pkscript: %w", err)
		}

		if !bytes.Equal(rec.PkScript, expectedPk) {
			return fmt.Errorf("vtxo pkscript mismatch")
		}

		if rec.Value != cp.Inputs[0].WitnessUtxo.Value {
			return fmt.Errorf("vtxo amount mismatch")
		}

		ownerLeaf, err := findOwnerLeafScript(
			ark, cp.UnsignedTx.TxHash(), checkpointPolicy,
		)
		if err != nil {
			return err
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
			return err
		}

		checkpointOut, err := artifact.ToCheckpointOutput()
		if err != nil {
			return err
		}

		// Compare rebuilt checkpoint txid to submitted checkpoint txid.
		if checkpointOut.Txid != cp.UnsignedTx.TxHash() {
			return fmt.Errorf("checkpoint txid mismatch")
		}

		checkpointOuts = append(checkpointOuts, checkpointOut)
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

	if rebuiltArk.UnsignedTx.TxHash() != ark.UnsignedTx.TxHash() {
		return fmt.Errorf("ark txid mismatch")
	}

	return nil
}

// findOwnerLeafScript extracts the owner leaf script for a specific checkpoint
// input by decoding the Ark input tap tree and removing the operator CSV leaf.
//
// The checkpoint tap tree for v0 is a two-leaf tree:
//   - operator-controlled CSV unroll leaf (derived from policy)
//   - owner collaborative leaf (selected by the client)
//
// This function ensures the Ark input carries the owner leaf in its PSBT
// metadata so rebuild validation can be deterministic.
func findOwnerLeafScript(ark *psbt.Packet, checkpointTxid chainhash.Hash,
	policy scripts.CheckpointPolicy) ([]byte, error) {

	for i, txIn := range ark.UnsignedTx.TxIn {
		if txIn.PreviousOutPoint.Hash != checkpointTxid {
			continue
		}

		if policy.OperatorKey == nil {
			return nil, fmt.Errorf("checkpoint operator key " +
				"must be provided")
		}

		encoded, err := oorlib.GetTapTreePSBTInput(ark.Inputs[i])
		if err != nil {
			return nil, err
		}

		leaves, err := oorlib.DecodeTapTree(encoded)
		if err != nil {
			return nil, err
		}

		unrollLeaf, err := scripts.UnilateralCSVTimeoutTapLeaf(
			policy.OperatorKey, policy.CSVDelay,
		)
		if err != nil {
			return nil, fmt.Errorf("unroll leaf: %w", err)
		}

		unrollScript := unrollLeaf.Script
		var (
			ownerLeaf   []byte
			foundUnroll bool
		)
		for _, leaf := range leaves {
			if bytes.Equal(leaf, unrollScript) {
				foundUnroll = true
				continue
			}

			if ownerLeaf != nil {
				return nil, fmt.Errorf("multiple owner leaves")
			}

			ownerLeaf = leaf
		}

		if ownerLeaf == nil {
			return nil, fmt.Errorf("owner leaf not found")
		}

		if !foundUnroll {
			return nil, fmt.Errorf("unroll leaf not found")
		}

		if len(ark.Inputs[i].TaprootLeafScript) == 0 {
			return nil, fmt.Errorf("missing ark tapleaf script")
		}

		hasOwnerLeaf := false
		for _, leaf := range ark.Inputs[i].TaprootLeafScript {
			if leaf == nil {
				continue
			}
			if bytes.Equal(leaf.Script, ownerLeaf) {
				hasOwnerLeaf = true
				break
			}
		}

		if !hasOwnerLeaf {
			return nil, fmt.Errorf("ark input does not use " +
				"owner leaf")
		}

		return ownerLeaf, nil
	}

	return nil, fmt.Errorf("ark input for checkpoint %s not found",
		checkpointTxid)
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
	for _, out := range ark.UnsignedTx.TxOut {
		if bytes.Equal(out.PkScript, scripts.AnchorOutput().PkScript) {
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
func extractArkRecipientsForRebuild(ark *psbt.Packet) (
	[]oorlib.RecipientOutput, error) {

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
