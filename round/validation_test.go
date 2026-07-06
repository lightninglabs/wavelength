package round

import (
	"testing"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestValidateDelayParametersValid tests that valid delay parameters pass
// validation.
func TestValidateDelayParametersValid(t *testing.T) {
	t.Parallel()

	err := ValidateDelayParameters(1008, 144)
	require.NoError(t, err)
}

// TestValidateDelayParametersZeroSweepDelay tests that zero sweep delay is
// rejected.
func TestValidateDelayParametersZeroSweepDelay(t *testing.T) {
	t.Parallel()

	err := ValidateDelayParameters(0, 144)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sweep delay is zero")
}

// TestValidateDelayParametersZeroExitDelay tests that zero exit delay is
// rejected.
func TestValidateDelayParametersZeroExitDelay(t *testing.T) {
	t.Parallel()

	err := ValidateDelayParameters(1008, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "VTXO exit delay is zero")
}

// TestValidateDelayParametersSweepNotGreaterThanExit tests that sweep delay
// must strictly exceed exit delay.
func TestValidateDelayParametersSweepNotGreaterThanExit(t *testing.T) {
	t.Parallel()

	// Sweep delay must strictly exceed exit delay to ensure users have time
	// to respond after the exit path becomes available.
	err := ValidateDelayParameters(144, 144)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be greater than")

	err = ValidateDelayParameters(100, 144)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be greater than")
}

// TestValidateDelayParametersSweepDelayTooLarge tests that sweep delay
// exceeding the maximum is rejected.
func TestValidateDelayParametersSweepDelayTooLarge(t *testing.T) {
	t.Parallel()

	err := ValidateDelayParameters(60000, 144)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds maximum reasonable")
}

// TestValidateDelayParametersExitDelayTooLarge tests that exit delay exceeding
// the maximum is rejected.
func TestValidateDelayParametersExitDelayTooLarge(t *testing.T) {
	t.Parallel()

	err := ValidateDelayParameters(100000, 60000)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds maximum reasonable")
}

// TestValidateDelayParametersBoundaryValues tests delay parameter validation
// at boundary values.
func TestValidateDelayParametersBoundaryValues(t *testing.T) {
	t.Parallel()

	// Maximum reasonable delay is 52560 blocks (approximately 1 year).
	err := ValidateDelayParameters(52560, 52559)
	require.NoError(t, err)

	err = ValidateDelayParameters(52561, 144)
	require.Error(t, err)
}

// TestValidateBoardingScriptNilTapscript tests that nil tapscript is rejected.
func TestValidateBoardingScriptNilTapscript(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	err := ValidateBoardingScript(
		nil, h.clientPubKey, h.operatorPubKey, testExitDelay,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "tapscript is nil")
}

// TestValidateBoardingScriptNilControlBlock tests that nil control block is
// rejected.
func TestValidateBoardingScriptNilControlBlock(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	tapscript := &waddrmgr.Tapscript{
		ControlBlock: nil,
	}

	err := ValidateBoardingScript(
		tapscript, h.clientPubKey, h.operatorPubKey, testExitDelay,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "control block is nil")
}

// TestValidateBoardingScriptNilInternalKey tests that nil internal key is
// rejected.
func TestValidateBoardingScriptNilInternalKey(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	tapscript := &waddrmgr.Tapscript{
		ControlBlock: &txscript.ControlBlock{
			InternalKey: nil,
		},
	}

	err := ValidateBoardingScript(
		tapscript, h.clientPubKey, h.operatorPubKey, testExitDelay,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "internal key is nil")
}

// TestValidateBoardingScriptWrongInternalKey tests that non-NUMS internal key
// is rejected.
func TestValidateBoardingScriptWrongInternalKey(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Internal key must be the ARK NUMS key to prevent anyone from
	// spending via the key path, forcing all spends through arkscript.
	_, wrongKey := generateTestKeyPair(t)

	tapscript := &waddrmgr.Tapscript{
		ControlBlock: &txscript.ControlBlock{
			InternalKey: wrongKey,
		},
	}

	err := ValidateBoardingScript(
		tapscript, h.clientPubKey, h.operatorPubKey, testExitDelay,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not ARK NUMS key")
}

// TestValidateBoardingScriptNotFullTree tests that non-full-tree tapscript
// types are rejected.
func TestValidateBoardingScriptNotFullTree(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Boarding scripts must be TapscriptTypeFullTree, not partial tree or
	// control block only.
	tapscript := &waddrmgr.Tapscript{
		Type: waddrmgr.TapscriptTypePartialReveal,
		ControlBlock: &txscript.ControlBlock{
			InternalKey: &arkscript.ARKNUMSKey,
		},
		Leaves: []txscript.TapLeaf{
			{
				Script: []byte{
					0x01,
				},
			},
			{
				Script: []byte{
					0x02,
				},
			},
		},
	}

	err := ValidateBoardingScript(
		tapscript, h.clientPubKey, h.operatorPubKey, testExitDelay,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be TapscriptTypeFullTree")
}

// TestValidateBoardingScriptFullTreeWrongLeafCount tests that full tree with
// wrong leaf count is rejected.
func TestValidateBoardingScriptFullTreeWrongLeafCount(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Boarding scripts have exactly 2 leaves: operator sweep and client
	// exit paths.
	tapscript := &waddrmgr.Tapscript{
		Type: waddrmgr.TapscriptTypeFullTree,
		ControlBlock: &txscript.ControlBlock{
			InternalKey: &arkscript.ARKNUMSKey,
		},
		Leaves: []txscript.TapLeaf{
			{
				Script: []byte{
					0x01,
				},
			},
		},
	}

	err := ValidateBoardingScript(
		tapscript, h.clientPubKey, h.operatorPubKey, testExitDelay,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected 2")
}

// TestValidateDelayParametersTableDriven tests delay parameter validation
// using table-driven test cases.
func TestValidateDelayParametersTableDriven(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		sweepDelay    uint32
		vtxoExitDelay uint32
		expectError   bool
		errorMsg      string
	}{
		{
			name:          "valid typical values",
			sweepDelay:    1008,
			vtxoExitDelay: 144,
			expectError:   false,
		},
		{
			name:          "valid minimum difference",
			sweepDelay:    2,
			vtxoExitDelay: 1,
			expectError:   false,
		},
		{
			name:          "valid at boundary",
			sweepDelay:    52560,
			vtxoExitDelay: 52559,
			expectError:   false,
		},
		{
			name:          "zero sweep delay",
			sweepDelay:    0,
			vtxoExitDelay: 144,
			expectError:   true,
			errorMsg:      "sweep delay is zero",
		},
		{
			name:          "zero exit delay",
			sweepDelay:    1008,
			vtxoExitDelay: 0,
			expectError:   true,
			errorMsg:      "exit delay is zero",
		},
		{
			name:          "both zero",
			sweepDelay:    0,
			vtxoExitDelay: 0,
			expectError:   true,
			errorMsg:      "sweep delay is zero",
		},
		{
			name:          "sweep equals exit",
			sweepDelay:    144,
			vtxoExitDelay: 144,
			expectError:   true,
			errorMsg:      "must be greater than",
		},
		{
			name:          "sweep less than exit",
			sweepDelay:    100,
			vtxoExitDelay: 200,
			expectError:   true,
			errorMsg:      "must be greater than",
		},
		{
			name:          "sweep exceeds max",
			sweepDelay:    52561,
			vtxoExitDelay: 144,
			expectError:   true,
			errorMsg:      "exceeds maximum",
		},
		{
			name:          "exit exceeds max",
			sweepDelay:    100000,
			vtxoExitDelay: 60000,
			expectError:   true,
			errorMsg:      "exceeds maximum",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateDelayParameters(
				tc.sweepDelay, tc.vtxoExitDelay,
			)
			if tc.expectError {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.errorMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
