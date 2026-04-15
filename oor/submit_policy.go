package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
)

// validateSubmitCheckpointPolicy enforces the operator-configured static
// checkpoint policy (operator key + CSV delay) against the checkpoint PSBT
// output tap trees referenced by an Ark submit package.
//
// This intentionally does not validate any ownership/state-dependent policy
// checks (eg. "does this VTXO belong to the submitting user?"). Those checks
// require wallet state and remain at the outbox boundary.
func validateSubmitCheckpointPolicy(
	checkpoints []*psbt.Packet,
	policy arkscript.CheckpointPolicy) error {

	if len(checkpoints) == 0 {
		return fmt.Errorf("checkpoint psbts must be provided")
	}

	// The canonical checkpoint tree includes a unilateral CSV timeout leaf.
	// It is parameterized by operator key + CSV delay.
	// We validate that leaf exists in per-input tap-tree encoding.
	// Malformed checkpoints fail before any session side effects.
	timeoutLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		policy.OperatorKey, policy.CSVDelay,
	)
	if err != nil {
		return err
	}

	expected := timeoutLeaf.Script

	for i, checkpoint := range checkpoints {
		if checkpoint == nil || checkpoint.UnsignedTx == nil {
			return fmt.Errorf("checkpoint psbt %d must include "+
				"unsigned tx", i)
		}

		if len(checkpoint.Outputs) == 0 {
			return fmt.Errorf(
				"checkpoint psbt %d has no outputs", i,
			)
		}

		encoded := checkpoint.Outputs[0].TaprootTapTree
		if len(encoded) == 0 {
			return fmt.Errorf("checkpoint psbt %d missing output "+
				"tap tree metadata", i)
		}

		leaves, err := oorlib.DecodeTapTree(encoded)
		if err != nil {
			return fmt.Errorf("ark psbt input %d: %w", i, err)
		}

		found := false
		for _, leaf := range leaves {
			if bytes.Equal(leaf, expected) {
				found = true
				break
			}
		}

		if !found {
			return fmt.Errorf(
				"ark psbt input %d: checkpoint policy "+
					"mismatch (missing operator csv leaf)",
				i,
			)
		}
	}

	return nil
}
