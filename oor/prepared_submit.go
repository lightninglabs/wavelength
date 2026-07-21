package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/psbt/v2"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
)

// PreparedSubmitPackage is an OOR Bitcoin graph whose Taproot Asset
// commitments were already inserted by an external tap-sdk orchestration
// boundary. The deterministic FSM never calls tapd; it validates and persists
// this immutable handoff before requesting any Bitcoin signature.
type PreparedSubmitPackage struct {
	// ArkPSBT is the committed checkpoint-to-recipient anchor transaction.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the committed input-to-checkpoint anchor
	// transactions, in transfer-input order.
	CheckpointPSBTs []*psbt.Packet

	// TaprootAssetTransfer carries the sealed tap-sdk package for every
	// checkpoint transition and the final Ark transition.
	TaprootAssetTransfer *oortx.TaprootAssetTransfer
}

// Validate binds the prepared Bitcoin graph and sealed asset packages to the
// caller's transfer inputs and canonical recipient metadata.
func (p *PreparedSubmitPackage) Validate(inputs []TransferInput,
	recipients []oortx.RecipientOutput) error {

	if p == nil {
		return fmt.Errorf("prepared submit package must be provided")
	}
	if len(inputs) == 0 || len(inputs) != len(p.CheckpointPSBTs) {
		return fmt.Errorf("prepared checkpoint count does not match " +
			"inputs")
	}
	if len(recipients) == 0 {
		return fmt.Errorf("prepared recipients must be provided")
	}
	if p.TaprootAssetTransfer == nil {
		return fmt.Errorf("prepared taproot asset transfer is required")
	}
	if err := p.TaprootAssetTransfer.Validate(
		len(p.CheckpointPSBTs),
	); err != nil {
		return err
	}
	if _, err := oortx.ValidateSubmitPackage(
		p.ArkPSBT, p.CheckpointPSBTs,
	); err != nil {
		return err
	}

	for i := range inputs {
		if err := inputs[i].Validate(); err != nil {
			return fmt.Errorf("prepared input %d: %w", i, err)
		}
		hasAssetRoot := inputs[i].TaprootAssetRoot != nil
		hasAssetPackage := len(
			p.TaprootAssetTransfer.CheckpointPackages[i],
		) != 0
		if hasAssetRoot != hasAssetPackage {
			return fmt.Errorf("prepared input %d asset root and "+
				"checkpoint package presence mismatch", i)
		}

		checkpoint := p.CheckpointPSBTs[i]
		if len(checkpoint.UnsignedTx.TxIn) != 1 {
			return fmt.Errorf("prepared checkpoint %d must have "+
				"one input", i)
		}
		if checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint !=
			inputs[i].VTXO.Outpoint {
			return fmt.Errorf("prepared checkpoint %d input "+
				"mismatch", i)
		}
	}

	canonicalRecipients := oortx.CanonicalRecipientOutputs(recipients)
	actualRecipients, err := ExtractArkRecipients(p.ArkPSBT)
	if err != nil {
		return err
	}
	if len(actualRecipients) != len(canonicalRecipients) {
		return fmt.Errorf("prepared recipient count mismatch")
	}
	assetRecipients := 0
	for i := range canonicalRecipients {
		recipient := canonicalRecipients[i]
		if recipient.TaprootAssetRoot != nil {
			assetRecipients++
		}
		err := recipient.ValidateTaprootAssetCommitment()
		if err != nil {
			return fmt.Errorf("prepared recipient %d: %w", i, err)
		}
		actual := actualRecipients[i]
		if actual.Value != recipient.Value ||
			!bytes.Equal(actual.PkScript, recipient.PkScript) {
			return fmt.Errorf("prepared recipient %d output "+
				"mismatch", i)
		}
	}
	if assetRecipients == 0 {
		return fmt.Errorf("prepared recipients require at least one " +
			"asset-bearing output")
	}

	return nil
}
