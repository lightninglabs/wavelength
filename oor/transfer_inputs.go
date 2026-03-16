package oor

import (
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/vtxo"
)

// TransferInput describes a spendable VTXO being used as an input to an
// outgoing OOR transfer.
//
// The VTXO descriptor provides everything needed for client-side signing
// (key descriptor + tapscript). The OwnerLeafScript is the draft
// checkpoint output leaf script committed to in the checkpoint output
// tap tree.
type TransferInput struct {
	// VTXO is the descriptor for the input VTXO being transferred.
	VTXO *vtxo.Descriptor

	// OwnerLeafScript is the leaf script committed to the checkpoint
	// tap tree. When empty and VTXO.ClientKey + VTXO.OperatorKey are
	// set, it is auto-derived via MultiSigCollabTapLeaf.
	OwnerLeafScript []byte

	// CustomSpend overrides the default collaborative leaf signing
	// for non-standard VTXOs (e.g., vHTLC Claim path). When set,
	// checkpoint signing uses this spend path directly.
	CustomSpend *arkscript.SpendPath
}

// Validate performs basic structural validation. For custom spend
// paths, the TapScript and ClientKey requirements are relaxed since
// the spend path carries its own signing context.
func (i *TransferInput) Validate() error {
	switch {
	case i == nil:
		return fmt.Errorf("transfer input must be provided")

	case i.VTXO == nil:
		return fmt.Errorf("vtxo must be provided")

	case i.VTXO.Amount <= 0:
		return fmt.Errorf("vtxo amount must be positive")

	case len(i.VTXO.PkScript) == 0:
		return fmt.Errorf("vtxo pkScript must be provided")

	case !i.IsCustomSpend() && i.VTXO.TapScript == nil:
		return fmt.Errorf("vtxo tapscript must be provided")

	case !i.IsCustomSpend() && i.VTXO.ClientKey.PubKey == nil:
		return fmt.Errorf("vtxo client key must be provided")
	}

	// Auto-derive OwnerLeafScript if not set.
	if len(i.OwnerLeafScript) == 0 {
		if i.VTXO.ClientKey.PubKey != nil &&
			i.VTXO.OperatorKey != nil {

			leaf, err := arkscript.MultiSigCollabTapLeaf(
				i.VTXO.ClientKey.PubKey,
				i.VTXO.OperatorKey,
			)
			if err != nil {
				return fmt.Errorf("derive owner leaf: %w",
					err)
			}

			i.OwnerLeafScript = leaf.Script
		} else {
			return fmt.Errorf("owner leaf script must be " +
				"provided")
		}
	}

	return nil
}

// IsCustomSpend returns true when this input uses a non-standard spend
// path (e.g., vHTLC Claim) rather than the default collaborative VTXO
// leaf.
func (i *TransferInput) IsCustomSpend() bool {
	return i.CustomSpend != nil
}

// CheckpointInput converts the OOR transfer input into the common tx
// builder checkpoint input type.
func (i *TransferInput) CheckpointInput() (oortx.CheckpointInput,
	error) {

	err := i.Validate()
	if err != nil {
		return oortx.CheckpointInput{}, err
	}

	return oortx.CheckpointInput{
		SpentVTXO: oortx.SpentVTXORef{
			Outpoint: i.VTXO.Outpoint,
			Output: &wire.TxOut{
				Value:    int64(i.VTXO.Amount),
				PkScript: i.VTXO.PkScript,
			},
		},
		OwnerLeafScript: i.OwnerLeafScript,
	}, nil
}
