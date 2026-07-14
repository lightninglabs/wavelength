package round

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/wavelength/lib/arkscript"
)

// MaxReasonableDelay is the maximum reasonable delay in blocks (~1 year).
const MaxReasonableDelay = 52560

// ValidateBoardingScript validates that a boarding tapscript has the
// expected structure with collaborative and timeout paths.
func ValidateBoardingScript(tapscript *waddrmgr.Tapscript,
	clientKey *btcec.PublicKey, operatorKey *btcec.PublicKey,
	expectedExitDelay uint32) error {

	if tapscript == nil {
		return fmt.Errorf("tapscript is nil")
	}

	// Ensure control block is present for taproot spending.
	if tapscript.ControlBlock == nil {
		return fmt.Errorf("tapscript control block is nil")
	}

	// Verify the internal key exists for taproot construction.
	if tapscript.ControlBlock.InternalKey == nil {
		return fmt.Errorf("control block internal key is nil")
	}

	// Ensure the internal key is unspendable (ARK NUMS key) to force
	// script path spending only.
	if !tapscript.ControlBlock.InternalKey.IsEqual(&arkscript.ARKNUMSKey) {
		return fmt.Errorf("internal key is not ARK NUMS key")
	}

	// Boarding scripts must be full tree types with both the collaborative
	// and timeout script paths.
	if tapscript.Type != waddrmgr.TapscriptTypeFullTree {
		return fmt.Errorf("boarding script must be "+
			"TapscriptTypeFullTree, got %v", tapscript.Type)
	}

	if len(tapscript.Leaves) != 2 {
		return fmt.Errorf("boarding script has %d leaves, expected 2",
			len(tapscript.Leaves))
	}

	// Construct the expected boarding tapscript using lib function. This
	// ensures we validate against the exact script structure that lib
	// creates.
	expectedTapscript, err := arkscript.VTXOTapScript(
		clientKey, operatorKey, expectedExitDelay,
	)
	if err != nil {
		return fmt.Errorf("failed to construct expected boarding "+
			"script: %w", err)
	}

	if len(expectedTapscript.Leaves) != 2 {
		return fmt.Errorf("expected tapscript has %d leaves, "+
			"should be 2", len(expectedTapscript.Leaves))
	}

	// Compare each leaf script byte-for-byte. The order may vary, so check
	// if each actual leaf matches one of expected leaves.
	actualLeaves := make(map[string]bool)
	for _, leaf := range tapscript.Leaves {
		actualLeaves[string(leaf.Script)] = true
	}

	for i, expectedLeaf := range expectedTapscript.Leaves {
		if !actualLeaves[string(expectedLeaf.Script)] {
			return fmt.Errorf("expected leaf %d not found in "+
				"actual boarding script", i)
		}
	}

	return nil
}

// ValidateDelayParameters validates the delay parameters for security.
//
// SweepDelay MUST be greater than VTXOExitDelay to ensure the operator has
// time to respond to unilateral exits before the batch expires.
func ValidateDelayParameters(sweepDelay, vtxoExitDelay uint32) error {
	// Both delays must be non-zero.
	if sweepDelay == 0 {
		return fmt.Errorf("sweep delay is zero")
	}
	if vtxoExitDelay == 0 {
		return fmt.Errorf("VTXO exit delay is zero")
	}

	// Sweep delay must be greater than VTXO exit delay for security.
	// This ensures the operator has time to respond to griefing attacks.
	if sweepDelay <= vtxoExitDelay {
		return fmt.Errorf("sweep delay (%d) must be greater than VTXO "+
			"exit delay (%d)", sweepDelay, vtxoExitDelay)
	}

	// Sanity check: Delays should be reasonable (less than ~1 year).
	if sweepDelay > MaxReasonableDelay {
		return fmt.Errorf("sweep delay (%d) exceeds maximum "+
			"reasonable value (%d blocks)", sweepDelay,
			MaxReasonableDelay)
	}
	if vtxoExitDelay > MaxReasonableDelay {
		return fmt.Errorf("VTXO exit delay (%d) exceeds maximum "+
			"reasonable value (%d blocks)", vtxoExitDelay,
			MaxReasonableDelay)
	}

	return nil
}
