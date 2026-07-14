package oor

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
)

// ArkRecipientOutput is a non-anchor Ark tx output intended for the receiver.
type ArkRecipientOutput struct {
	// OutputIndex is the index of this output in the Ark tx.
	OutputIndex uint32

	// Value is the output amount in satoshis.
	Value btcutil.Amount

	// PkScript is the raw pkScript bytes.
	PkScript []byte

	// VTXOPolicyTemplate is the serialized arkscript policy template for
	// the VTXO created by this output, when the server supplied it.
	VTXOPolicyTemplate []byte
}

// ExtractArkRecipients returns the non-anchor outputs from a canonical Ark
// PSBT, preserving their transaction indices.
//
// This helper is intentionally structural. It does not attempt to map outputs
// into VTXO descriptors (that requires closure/script semantics).
//
// Canonical ordering is required so output indices are stable: recipients can
// reference outputs by index without ambiguity (e.g. for event logs and
// materialization).
func ExtractArkRecipients(ark *psbt.Packet) ([]ArkRecipientOutput, error) {
	if ark == nil || ark.UnsignedTx == nil {
		return nil, fmt.Errorf("ark psbt must be provided")
	}

	err := arktx.ValidateCanonicalPSBT(ark)
	if err != nil {
		return nil, err
	}

	tx := ark.UnsignedTx

	recipients := make([]ArkRecipientOutput, 0, len(tx.TxOut))
	for idx, out := range tx.TxOut {
		if arktx.IsAnchorOutput(out) {
			continue
		}

		recipients = append(recipients, ArkRecipientOutput{
			OutputIndex: uint32(idx),
			Value:       btcutil.Amount(out.Value),
			PkScript:    out.PkScript,
		})
	}

	if len(recipients) == 0 {
		return nil, fmt.Errorf("ark tx has no recipient outputs")
	}

	return recipients, nil
}

// CloneArkRecipients deep-copies recipient outputs and their optional policy
// metadata.
func CloneArkRecipients(recipients []ArkRecipientOutput) []ArkRecipientOutput {
	if len(recipients) == 0 {
		return nil
	}

	out := make([]ArkRecipientOutput, len(recipients))
	for i := range recipients {
		out[i] = ArkRecipientOutput{
			OutputIndex: recipients[i].OutputIndex,
			Value:       recipients[i].Value,
			PkScript: append(
				[]byte(nil), recipients[i].PkScript...,
			),
			VTXOPolicyTemplate: append(
				[]byte(nil),
				recipients[i].VTXOPolicyTemplate...,
			),
		}
	}

	return out
}
