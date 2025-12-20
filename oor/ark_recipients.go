package oor

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
)

// ArkRecipientOutput is a non-anchor Ark tx output intended for the receiver.
type ArkRecipientOutput struct {
	// OutputIndex is the index of this output in the Ark tx.
	OutputIndex uint32

	// Value is the output amount in satoshis.
	Value btcutil.Amount

	// PkScript is the raw pkScript bytes.
	PkScript []byte
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

	err := oortx.ValidateCanonicalArkPSBT(ark)
	if err != nil {
		return nil, err
	}

	tx := ark.UnsignedTx

	recipients := make([]ArkRecipientOutput, 0, len(tx.TxOut))
	for idx, out := range tx.TxOut {
		if oortx.IsAnchorOutput(out) {
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
