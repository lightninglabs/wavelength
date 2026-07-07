package oor

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightninglabs/darepo-client/lib/tx/checkpoint"
)

// CheckpointInput describes the VTXO input being transformed into a checkpoint
// output for an OOR transfer.
//
// "Owner" in the nested leaf script naming means owner of the spent VTXO
// input, not owner of the checkpoint CSV timeout path.
type CheckpointInput = checkpoint.Input

// SpentVTXORef groups the spent VTXO outpoint and output data used to build a
// checkpoint input.
type SpentVTXORef = checkpoint.SpentVTXORef

// CheckpointArtifact is the submit-phase checkpoint artifact.
//
// The checkpoint tap tree metadata is carried as sidecar bytes in this phase
// so callers can derive tapleaf proofs directly from the build result.
type CheckpointArtifact struct {
	// PSBT is the checkpoint transaction PSBT.
	PSBT *psbt.Packet

	// TapTreeEncoded is the v0 tap tree encoding for the checkpoint output.
	TapTreeEncoded []byte

	// OwnerLeafScript is the canonical owner leaf committed to the
	// checkpoint output.
	OwnerLeafScript []byte

	// OwnerLeafPolicy is the semantic owner-leaf policy encoding.
	OwnerLeafPolicy []byte
}

// ToCheckpointOutput projects this artifact into the Ark-builder checkpoint
// input shape.
func (a *CheckpointArtifact) ToCheckpointOutput() (CheckpointOutput, error) {
	if a == nil || a.PSBT == nil || a.PSBT.UnsignedTx == nil {
		return CheckpointOutput{}, fmt.Errorf("checkpoint psbt must " +
			"be provided")
	}

	if err := validateCheckpointTx(a.PSBT.UnsignedTx); err != nil {
		return CheckpointOutput{}, err
	}

	return CheckpointOutput{
		Txid:            a.PSBT.UnsignedTx.TxHash(),
		Output:          a.PSBT.UnsignedTx.TxOut[0],
		TapTreeEncoded:  a.TapTreeEncoded,
		OwnerLeafScript: a.OwnerLeafScript,
		OwnerLeafPolicy: a.OwnerLeafPolicy,
	}, nil
}

// RecipientOutput describes an Ark tx recipient output.
type RecipientOutput struct {
	// PkScript is the destination script.
	PkScript []byte

	// Value is the amount to send in satoshis.
	Value btcutil.Amount

	// VTXOPolicyTemplate is the semantic arkscript policy for the output
	// when the recipient is a non-standard script (for example, a vHTLC).
	// This metadata does not affect tx construction but lets downstream
	// services persist authoritative ownership semantics for the created
	// VTXO.
	VTXOPolicyTemplate []byte
}

// CanonicalRecipientOutputs returns a BIP69-style stable copy of recipients in
// the same order used by BuildArkPSBT.
func CanonicalRecipientOutputs(recipients []RecipientOutput) []RecipientOutput {
	out := make([]RecipientOutput, len(recipients))
	copy(out, recipients)

	sort.SliceStable(out, func(i, j int) bool {
		a := out[i]
		b := out[j]

		if a.Value != b.Value {
			return a.Value < b.Value
		}

		return bytes.Compare(a.PkScript, b.PkScript) < 0
	})

	return out
}

// RecipientOutputIndex returns the tx output index for target after applying
// the same canonical recipient ordering used by BuildArkPSBT.
func RecipientOutputIndex(recipients []RecipientOutput,
	target RecipientOutput) (uint32, error) {

	var (
		found bool
		index uint32
	)

	for i, recipient := range CanonicalRecipientOutputs(recipients) {
		if recipient.Value != target.Value ||
			!bytes.Equal(recipient.PkScript, target.PkScript) {

			continue
		}

		if found {
			return 0, fmt.Errorf("recipient output is ambiguous")
		}

		found = true
		index = uint32(i)
	}

	if !found {
		return 0, fmt.Errorf("recipient output not found")
	}

	return index, nil
}

// RecipientOutPoint returns the resolved Ark tx outpoint for target after
// applying BuildArkPSBT recipient ordering.
func RecipientOutPoint(txid chainhash.Hash, recipients []RecipientOutput,
	target RecipientOutput) (wire.OutPoint, error) {

	index, err := RecipientOutputIndex(recipients, target)
	if err != nil {
		return wire.OutPoint{}, err
	}

	return wire.OutPoint{
		Hash:  txid,
		Index: index,
	}, nil
}

// BuildCheckpointPSBT constructs an unsigned checkpoint PSBT that spends a
// VTXO input, pays the entire input value to a checkpoint P2TR output, and
// appends a zero-value anchor output.
//
// The checkpoint output pkScript is derived deterministically from:
//
// - the operator checkpoint policy, and
// - the caller-provided VTXO-owner collaborative leaf script.
//
// This function does not attempt to sign the checkpoint tx. It also does not
// validate that the owner leaf is a canonical Ark closure (draft phase).
func BuildCheckpointPSBT(policy arkscript.CheckpointPolicy,
	in CheckpointInput) (*CheckpointArtifact, error) {

	result, err := checkpoint.BuildPSBT(policy, in)
	if err != nil {
		return nil, err
	}

	return &CheckpointArtifact{
		PSBT:            result.PSBT,
		TapTreeEncoded:  result.TapTreeEncoded,
		OwnerLeafScript: result.OwnerLeafScript,
		OwnerLeafPolicy: result.OwnerLeafPolicy,
	}, nil
}

// CheckpointOutput describes a checkpoint output that will be spent by the Ark
// transaction.
type CheckpointOutput struct {
	// Txid is the txid of the checkpoint transaction.
	Txid chainhash.Hash

	// Output is the checkpoint output being spent (value + pkScript).
	Output *wire.TxOut

	// TapTreeEncoded is the v0 tap tree encoding for the checkpoint output.
	TapTreeEncoded []byte

	// OwnerLeafScript is the canonical owner leaf committed to the
	// checkpoint output.
	OwnerLeafScript []byte

	// OwnerLeafPolicy is the semantic owner-leaf policy encoding.
	OwnerLeafPolicy []byte
}

// BuildArkPSBT constructs a deterministic Ark tx PSBT spending the set of
// checkpoint outputs and producing the requested recipient outputs plus an
// anchor output.
//
// This is a v0 builder and enforces:
//
// - fee-less transfers (sum(inputs) == sum(outputs excluding anchor)),
// - anchor output is last output (P2A, value 0), and
// - canonical ordering rules for inputs/outputs (BIP69),
//
// The checkpoint output's standard PSBT tap tree metadata remains the
// authoritative source of tree data. Ark inputs only carry the prevout
// WitnessUtxo.
func BuildArkPSBT(checkpoints []CheckpointOutput,
	recipients []RecipientOutput) (*psbt.Packet, error) {

	if len(checkpoints) == 0 {
		return nil, fmt.Errorf("checkpoint outputs must be provided")
	}

	if len(recipients) == 0 {
		return nil, fmt.Errorf("recipient outputs must be provided")
	}

	// Sum inputs/outputs and enforce a fee-less OOR transfer in v0.
	//
	// For now we intentionally do not support operator fee outputs because
	// they complicate minimum relay/dust constraints and also shift policy
	// questions into the transfer flow. Fees can be introduced elsewhere
	// (for example, cooperative exit).
	var sumInputs btcutil.Amount
	for _, cp := range checkpoints {
		if cp.Output == nil {
			return nil, fmt.Errorf("checkpoint output must be " +
				"provided")
		}

		if len(cp.Output.PkScript) == 0 {
			return nil, fmt.Errorf("checkpoint pkScript must be " +
				"provided")
		}

		if cp.Output.Value <= 0 {
			return nil, fmt.Errorf("checkpoint output value must " +
				"be positive")
		}

		sumInputs += btcutil.Amount(cp.Output.Value)
	}

	var sumOutputs btcutil.Amount
	for _, out := range recipients {
		if len(out.PkScript) == 0 {
			return nil, fmt.Errorf("recipient pkScript must be " +
				"provided")
		}

		if out.Value <= 0 {
			return nil, fmt.Errorf("recipient value must be " +
				"positive")
		}

		sumOutputs += out.Value
	}

	if sumInputs != sumOutputs {
		return nil, fmt.Errorf("fee-less ark tx requires equal " +
			"input/output sums")
	}

	// Sort checkpoint inputs by outpoint (BIP69-style) to ensure a stable
	// txid and stable session id across restarts and retries.
	checkpointsSorted := make([]CheckpointOutput, len(checkpoints))
	copy(checkpointsSorted, checkpoints)
	sort.SliceStable(checkpointsSorted, func(i, j int) bool {
		a := checkpointsSorted[i]
		b := checkpointsSorted[j]

		cmp := bytes.Compare(a.Txid[:], b.Txid[:])
		if cmp != 0 {
			return cmp < 0
		}

		// v0 always spends vout=0.
		return false
	})

	recipientOuts := CanonicalRecipientOutputs(recipients)

	tx := wire.NewMsgTx(arktx.TxVersion)
	for _, cp := range checkpointsSorted {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash:  cp.Txid,
				Index: 0,
			},
			Sequence: wire.MaxTxInSequenceNum,
		})
	}

	for _, out := range recipientOuts {
		tx.AddTxOut(&wire.TxOut{
			Value:    int64(out.Value),
			PkScript: out.PkScript,
		})
	}

	tx.AddTxOut(arkscript.AnchorOutput())

	err := arktx.ValidateCanonicalTx(tx)
	if err != nil {
		return nil, fmt.Errorf("internal: built ark tx is not "+
			"canonical: %w", err)
	}

	pkt, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, fmt.Errorf("unable to create ark psbt: %w", err)
	}

	// Attach witness UTXOs in the same order as inputs.
	for i := range checkpointsSorted {
		cp := checkpointsSorted[i]

		pkt.Inputs[i].WitnessUtxo = cp.Output
	}

	return pkt, nil
}
