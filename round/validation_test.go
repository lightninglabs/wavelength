package round

import (
	"testing"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/stretchr/testify/require"
)

func TestValidateDelayParametersValid(t *testing.T) {
	t.Parallel()

	err := ValidateDelayParameters(1008, 144)
	require.NoError(t, err)
}

func TestValidateDelayParametersZeroSweepDelay(t *testing.T) {
	t.Parallel()

	err := ValidateDelayParameters(0, 144)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sweep delay is zero")
}

func TestValidateDelayParametersZeroExitDelay(t *testing.T) {
	t.Parallel()

	err := ValidateDelayParameters(1008, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "VTXO exit delay is zero")
}

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

func TestValidateDelayParametersSweepDelayTooLarge(t *testing.T) {
	t.Parallel()

	err := ValidateDelayParameters(60000, 144)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds maximum reasonable")
}

func TestValidateDelayParametersExitDelayTooLarge(t *testing.T) {
	t.Parallel()

	err := ValidateDelayParameters(100000, 60000)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds maximum reasonable")
}

func TestValidateDelayParametersBoundaryValues(t *testing.T) {
	t.Parallel()

	// Maximum reasonable delay is 52560 blocks (approximately 1 year).
	err := ValidateDelayParameters(52560, 52559)
	require.NoError(t, err)

	err = ValidateDelayParameters(52561, 144)
	require.Error(t, err)
}

func TestValidateBoardingSignatureNilCommitmentTx(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	intent := h.newTestBoardingIntent()

	err := ValidateBoardingSignature(
		nil, &intent.BoardingRequest, make([]byte, 64), 0,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "commitment tx is nil")
}

func TestValidateBoardingSignatureNilBoardingRequest(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	intent := h.newTestBoardingIntent()
	tx := h.newTestCommitmentTx([]wire.OutPoint{intent.Outpoint})

	err := ValidateBoardingSignature(tx, nil, make([]byte, 64), 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "boarding request is nil")
}

func TestValidateBoardingSignatureInvalidLength(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	intent := h.newTestBoardingIntent()
	tx := h.newTestCommitmentTx([]wire.OutPoint{intent.Outpoint})

	// Schnorr signatures must be exactly 64 bytes.
	err := ValidateBoardingSignature(
		tx, &intent.BoardingRequest, make([]byte, 32), 0,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid signature length")

	err = ValidateBoardingSignature(
		tx, &intent.BoardingRequest, make([]byte, 128), 0,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid signature length")
}

func TestValidateBoardingSignatureInputIndexOutOfRange(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	intent := h.newTestBoardingIntent()
	tx := h.newTestCommitmentTx([]wire.OutPoint{intent.Outpoint})

	err := ValidateBoardingSignature(
		tx, &intent.BoardingRequest, make([]byte, 64), -1,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "input index")

	err = ValidateBoardingSignature(
		tx, &intent.BoardingRequest, make([]byte, 64), 5,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "out of range")
}

func TestValidateBoardingSignatureInputMismatch(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	intent := h.newTestBoardingIntent()

	// Signature must be for the specific boarding UTXO being committed.
	differentOutpoint := h.newTestOutpoint()
	tx := h.newTestCommitmentTx([]wire.OutPoint{differentOutpoint})

	err := ValidateBoardingSignature(
		tx, &intent.BoardingRequest, make([]byte, 64), 0,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not reference boarding UTXO")
}

func TestValidateBoardingSignatureValid(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	intent := h.newTestBoardingIntent()
	tx := h.newTestCommitmentTx([]wire.OutPoint{intent.Outpoint})

	// Signature structure validation only; cryptographic verification is
	// performed separately in the signing flow.
	err := ValidateBoardingSignature(
		tx, &intent.BoardingRequest, make([]byte, 64), 0,
	)
	require.NoError(t, err)
}

func TestValidateBoardingScriptNilTapscript(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	err := ValidateBoardingScript(
		nil, h.clientPubKey, h.operatorPubKey, testExitDelay,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "tapscript is nil")
}

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

func TestValidateBoardingScriptWrongInternalKey(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Internal key must be the ARK NUMS key to prevent anyone from
	// spending via the key path, forcing all spends through scripts.
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

func TestValidateBoardingScriptFullTreeWrongLeafCount(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Boarding scripts have exactly 2 leaves: operator sweep and client
	// exit paths.
	tapscript := &waddrmgr.Tapscript{
		Type: waddrmgr.TapscriptTypeFullTree,
		ControlBlock: &txscript.ControlBlock{
			InternalKey: &scripts.ARKNUMSKey,
		},
		Leaves: []txscript.TapLeaf{
			{Script: []byte{0x01}},
		},
	}

	err := ValidateBoardingScript(
		tapscript, h.clientPubKey, h.operatorPubKey, testExitDelay,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected 2")
}

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

func TestValidateBoardingSignatureTableDriven(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	intent := h.newTestBoardingIntent()

	testCases := []struct {
		name        string
		setupTx     func() *wire.MsgTx
		request     *types.BoardingRequest
		sigLen      int
		inputIndex  int
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid signature",
			setupTx: func() *wire.MsgTx {
				inputs := []wire.OutPoint{intent.Outpoint}
				return h.newTestCommitmentTx(inputs)
			},
			request:     &intent.BoardingRequest,
			sigLen:      64,
			inputIndex:  0,
			expectError: false,
		},
		{
			name: "nil commitment tx",
			setupTx: func() *wire.MsgTx {
				return nil
			},
			request:     &intent.BoardingRequest,
			sigLen:      64,
			inputIndex:  0,
			expectError: true,
			errorMsg:    "commitment tx is nil",
		},
		{
			name: "nil boarding request",
			setupTx: func() *wire.MsgTx {
				inputs := []wire.OutPoint{intent.Outpoint}
				return h.newTestCommitmentTx(inputs)
			},
			request:     nil,
			sigLen:      64,
			inputIndex:  0,
			expectError: true,
			errorMsg:    "boarding request is nil",
		},
		{
			name: "signature too short",
			setupTx: func() *wire.MsgTx {
				inputs := []wire.OutPoint{intent.Outpoint}
				return h.newTestCommitmentTx(inputs)
			},
			request:     &intent.BoardingRequest,
			sigLen:      32,
			inputIndex:  0,
			expectError: true,
			errorMsg:    "invalid signature length",
		},
		{
			name: "signature too long",
			setupTx: func() *wire.MsgTx {
				inputs := []wire.OutPoint{intent.Outpoint}
				return h.newTestCommitmentTx(inputs)
			},
			request:     &intent.BoardingRequest,
			sigLen:      128,
			inputIndex:  0,
			expectError: true,
			errorMsg:    "invalid signature length",
		},
		{
			name: "negative input index",
			setupTx: func() *wire.MsgTx {
				inputs := []wire.OutPoint{intent.Outpoint}
				return h.newTestCommitmentTx(inputs)
			},
			request:     &intent.BoardingRequest,
			sigLen:      64,
			inputIndex:  -1,
			expectError: true,
			errorMsg:    "out of range",
		},
		{
			name: "input index too large",
			setupTx: func() *wire.MsgTx {
				inputs := []wire.OutPoint{intent.Outpoint}
				return h.newTestCommitmentTx(inputs)
			},
			request:     &intent.BoardingRequest,
			sigLen:      64,
			inputIndex:  10,
			expectError: true,
			errorMsg:    "out of range",
		},
		{
			name: "input mismatch",
			setupTx: func() *wire.MsgTx {
				differentOutpoint := h.newTestOutpoint()
				inputs := []wire.OutPoint{differentOutpoint}
				return h.newTestCommitmentTx(inputs)
			},
			request:     &intent.BoardingRequest,
			sigLen:      64,
			inputIndex:  0,
			expectError: true,
			errorMsg:    "does not reference boarding UTXO",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tx := tc.setupTx()
			sig := make([]byte, tc.sigLen)

			err := ValidateBoardingSignature(
				tx, tc.request, sig, tc.inputIndex,
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
