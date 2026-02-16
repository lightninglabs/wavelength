package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
)

// validateSubmitCheckpointPolicy enforces the operator-configured static
// checkpoint policy (operator key + CSV delay) against the v0 per-input tap
// tree metadata attached to an Ark submit PSBT.
//
// This intentionally does not validate any ownership/state-dependent policy
// checks (eg. "does this VTXO belong to the submitting user?"). Those checks
// require wallet state and remain at the outbox boundary.
func validateSubmitCheckpointPolicy(ark *psbt.Packet,
	policy scripts.CheckpointPolicy) error {

	if ark == nil {
		return fmt.Errorf("ark psbt must be provided")
	}

	// The canonical checkpoint tree includes a unilateral CSV timeout leaf.
	// It is parameterized by operator key + CSV delay.
	// We validate that leaf exists in per-input tap-tree encoding.
	// Malformed checkpoints fail before any session side effects.
	timeoutLeaf, err := scripts.UnilateralCSVTimeoutTapLeaf(
		policy.OperatorKey, policy.CSVDelay,
	)
	if err != nil {
		return err
	}

	expected := timeoutLeaf.Script

	for i, in := range ark.Inputs {
		encoded, err := oorlib.GetTapTreePSBTInput(in)
		if err != nil {
			return fmt.Errorf("ark psbt input %d: %w", i, err)
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
