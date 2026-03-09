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
// The VTXO descriptor provides key material and the output script. SpendInfo
// carries the compiled leaf script and control block for the specific spend
// path; callers derive it via the arkscript scripting system (e.g.
// VTXOPolicy.CollabSpendInfo() for standard VTXOs or
// vhtlc.Policy.ClaimSpendInfo() for vHTLC inputs). OwnerLeafScript is the
// checkpoint output owner leaf committed to the tap tree.
type TransferInput struct {
	// VTXO is the descriptor for the input VTXO being transferred.
	VTXO *vtxo.Descriptor

	// OwnerLeafScript is the leaf script committed to the checkpoint tap
	// tree.
	//
	// This is currently a draft implementation, and may change as the
	// checkpoint policy is refined.
	OwnerLeafScript []byte

	// SpendInfo is the spend path the client (and server) use when signing
	// checkpoint PSBTs. Callers derive this via the arkscript system
	// (e.g. VTXOPolicy.CollabSpendInfo() or
	// vhtlc.Policy.ClaimSpendInfo()).
	SpendInfo *arkscript.SpendInfo

	// ConditionWitness contains extra witness items that must appear
	// between the signature stack and the leaf script. For example, a
	// HashLock-gated leaf requires the preimage here. Empty for standard
	// 2-of-2 collab paths.
	ConditionWitness [][]byte
}

// Validate performs basic structural validation.
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

	case i.VTXO.ClientKey.PubKey == nil:
		return fmt.Errorf("vtxo client key must be provided")

	case len(i.OwnerLeafScript) == 0:
		return fmt.Errorf("owner leaf script must be provided")
	}

	return nil
}

// CheckpointInput converts the OOR transfer input into the common tx builder
// checkpoint input type.
func (i *TransferInput) CheckpointInput() (oortx.CheckpointInput, error) {
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
