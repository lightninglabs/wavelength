package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
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

	// Asset carries the optional Taproot Asset commitment metadata when
	// this output creates an asset-bearing VTXO.
	Asset *oortx.RecipientAsset
}

// RoutingPkScript returns the uncomposed Ark receive script used to look up
// local receive-script ownership and indexer proof keys for this output. Plain
// Bitcoin outputs return PkScript unchanged; asset outputs verify that
// composing VTXOPolicyTemplate with Asset.Root produces PkScript and return
// the uncomposed policy script.
func (o ArkRecipientOutput) RoutingPkScript() ([]byte, error) {
	if o.Asset == nil {
		return bytes.Clone(o.PkScript), nil
	}
	if err := o.Asset.Validate(); err != nil {
		return nil, fmt.Errorf("recipient output %d asset: %w",
			o.OutputIndex, err)
	}
	if len(o.VTXOPolicyTemplate) == 0 {
		return nil, fmt.Errorf("recipient output %d asset requires "+
			"vtxo policy template", o.OutputIndex)
	}

	template, err := arkscript.DecodePolicyTemplate(o.VTXOPolicyTemplate)
	if err != nil {
		return nil, fmt.Errorf("decode recipient output %d policy: %w",
			o.OutputIndex, err)
	}
	compiled, err := template.Compile()
	if err != nil {
		return nil, fmt.Errorf("compile recipient output %d policy: %w",
			o.OutputIndex, err)
	}
	composed, err := arkscript.ComposeWithSiblingRoot(
		compiled, o.Asset.Root,
	)
	if err != nil {
		return nil, fmt.Errorf("compose recipient output %d asset "+
			"root: %w", o.OutputIndex, err)
	}
	composedPkScript, err := txscript.PayToTaprootScript(
		composed.OutputKey(),
	)
	if err != nil {
		return nil, fmt.Errorf("derive composed recipient output %d "+
			"pkscript: %w", o.OutputIndex, err)
	}
	if !bytes.Equal(composedPkScript, o.PkScript) {
		return nil, fmt.Errorf("recipient output %d composed policy "+
			"does not match pkscript", o.OutputIndex)
	}

	routingPkScript, err := txscript.PayToTaprootScript(
		compiled.OutputKey(),
	)
	if err != nil {
		return nil, fmt.Errorf("derive routing recipient output %d "+
			"pkscript: %w", o.OutputIndex, err)
	}

	return routingPkScript, nil
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
			Asset: recipients[i].Asset.Clone(),
		}
	}

	return out
}
