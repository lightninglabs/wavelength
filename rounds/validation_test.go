package rounds

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/tx"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// testPolicyTemplate encodes the standard policy shape used by these
// validation fixtures.
func testPolicyTemplate(t *testing.T, clientKey,
	operatorKey *btcec.PublicKey, exitDelay uint32) []byte {

	t.Helper()

	policy, err := arkscript.EncodeStandardVTXOTemplate(
		clientKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	return policy
}

// TestValidateBoardingRequest tests the ValidateBoardingRequest validation
// function with various scenarios.
func TestValidateBoardingRequest(t *testing.T) {
	t.Parallel()

	// Set up test fixtures.
	clientPub, _ := testutils.CreateKey(2)
	wrongOpPub, _ := testutils.CreateKey(3)

	outpoint1 := wire.OutPoint{
		Hash:  [32]byte{0x01},
		Index: 0,
	}

	t.Run("valid boarding request returns input", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Set up validation mocks.
		exitDelay := uint32(144)
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, exitDelay, 10,
		)

		req := &types.BoardingRequest{
			Outpoint: &outpoint1,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, exitDelay,
			),
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   exitDelay,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req, 100,
		)
		require.NoError(t, err)

		// Verify returned BoardingInput has expected values.
		require.NotNil(t, boardingInput)
		require.Equal(t, &outpoint1, boardingInput.Outpoint)
		require.Equal(t, req.ClientKey, boardingInput.ClientKey)
		require.NotNil(t, boardingInput.Tapscript)
		require.NotNil(t, boardingInput.PkScript)
		require.NotNil(t, boardingInput.OperatorKeyDesc)
	})

	t.Run("boarding input already locked", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Create a RoundID for the lock.
		otherRoundID, err := NewRoundID()
		require.NoError(t, err)

		// Mock: input is already locked by another round.
		h.lockBoardingInput(&outpoint1, otherRoundID)

		req := &types.BoardingRequest{
			Outpoint: &outpoint1,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, 144,
			),
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   144,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req, 100,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrBoardingInputLocked)
	})

	t.Run("is locked check fails", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Mock: IsLocked returns an error.
		h.boardingLocker.On("IsLocked", t.Context(), &outpoint1).
			Return(false, RoundID{}, fmt.Errorf("database error"))

		req := &types.BoardingRequest{
			Outpoint: &outpoint1,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, 144,
			),
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   144,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req, 100,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrCheckLockFailed)
	})

	t.Run("operator key mismatch", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Mock: no locks on this input.
		h.allowBoardingInput(&outpoint1)

		req := &types.BoardingRequest{
			Outpoint: &outpoint1,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, wrongOpPub, 144,
			),
			ClientKey: clientPub,
			// Wrong operator key.
			OperatorKey: wrongOpPub,
			ExitDelay:   144,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req, 100,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrOperatorKeyMismatch)
	})

	t.Run("exit delay below minimum", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Minimum exit delay of 200 blocks.
		h.env.Terms.BoardingExitDelay = 200

		// Mock: no locks.
		h.allowBoardingInput(&outpoint1)

		req := &types.BoardingRequest{
			Outpoint: &outpoint1,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, 100,
			),
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			// Less than minimum.
			ExitDelay: 100,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req, 100,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrExitDelayTooLow)
	})

	t.Run("utxo not found or spent", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Mock: no locks.
		h.allowBoardingInput(&outpoint1)

		// Mock ChainSource to return error (UTXO doesn't exist).
		h.chainSource.On("GetUTXO", outpoint1).
			Return(nil, fmt.Errorf("utxo not found"))

		req := &types.BoardingRequest{
			Outpoint: &outpoint1,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, 144,
			),
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   144,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req, 100,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrFetchUTXO)
	})

	t.Run("utxo confirmations below minimum", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.BoardingExitDelay = 100
		h.env.Terms.MinBoardingConfirmations = 10

		// Set up validation mocks with insufficient confirmations.
		exitDelay := uint32(144)
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, exitDelay, 5,
		)

		req := &types.BoardingRequest{
			Outpoint: &outpoint1,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, exitDelay,
			),
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   exitDelay,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req, 100,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrInsufficientConfirmations)
	})

	t.Run("pkscript mismatch", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Mock: no locks.
		h.allowBoardingInput(&outpoint1)

		// Mock ChainSource to return UTXO with wrong pkScript.
		// The UTXO has a different script than what we expect.
		utxo := &UTXO{
			Output: &wire.TxOut{
				Value:    100000,
				PkScript: []byte{0xde, 0xad, 0xbe, 0xef},
			},
			Confirmations: 10,
		}
		h.chainSource.On("GetUTXO", outpoint1).Return(utxo, nil)

		req := &types.BoardingRequest{
			Outpoint: &outpoint1,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, 144,
			),
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   144,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req, 100,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrPkScriptMismatch)
	})

	t.Run("delay path too close - at safety margin", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Exit delay is 100, safety margin is 6, so max safe is 94.
		// Set confirmations to exactly 94 (at the boundary).
		exitDelay := uint32(100)
		safetyMargin := h.env.Terms.BoardingExitDelaySafetyMargin
		confirmations := int64(exitDelay - safetyMargin)
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, exitDelay, confirmations,
		)

		req := &types.BoardingRequest{
			Outpoint: &outpoint1,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, exitDelay,
			),
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   exitDelay,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req, 100,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrDelayPathTooClose)
	})

	t.Run("delay path too close - past safety margin", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Exit delay is 100, set confirmations to 98 (well past
		// safety margin).
		exitDelay := uint32(100)
		confirmations := int64(98)
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, exitDelay, confirmations,
		)

		req := &types.BoardingRequest{
			Outpoint: &outpoint1,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, exitDelay,
			),
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   exitDelay,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req, 100,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrDelayPathTooClose)
	})

	t.Run("valid confirmations within safety margin", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Exit delay is 100, safety margin is 6, so max safe is 94.
		// Set confirmations to 93 (just below the threshold).
		exitDelay := uint32(100)
		confirmations := int64(93)
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, exitDelay, confirmations,
		)

		req := &types.BoardingRequest{
			Outpoint: &outpoint1,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, exitDelay,
			),
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   exitDelay,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req, 100,
		)
		require.NoError(t, err)
		require.NotNil(t, boardingInput)
	})

	// Guard against a misconfigured operator whose
	// BoardingExitDelaySafetyMargin is >= the policy's exit delay. The
	// old code computed `maxSafe := params.ExitDelay - safetyMargin`
	// without guarding, which on uint32 underflows to a huge value and
	// silently admitted the boarding input.
	t.Run("exit delay at safety margin rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Drop the configured minimum so the policy can set a short
		// delay without tripping ErrExitDelayTooLow first.
		h.env.Terms.BoardingExitDelay = 0

		// Policy exit delay equals the safety margin exactly -> the
		// safe confirmation window is empty -> must reject.
		safetyMargin := h.env.Terms.BoardingExitDelaySafetyMargin
		exitDelay := safetyMargin
		h.allowBoardingInput(&outpoint1)

		req := &types.BoardingRequest{
			Outpoint: &outpoint1,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, exitDelay,
			),
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   exitDelay,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req, 100,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrExitDelayBelowSafetyMargin)
	})

	t.Run("exit delay below safety margin rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.BoardingExitDelay = 0

		safetyMargin := h.env.Terms.BoardingExitDelaySafetyMargin
		exitDelay := safetyMargin - 1
		h.allowBoardingInput(&outpoint1)

		req := &types.BoardingRequest{
			Outpoint: &outpoint1,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, exitDelay,
			),
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   exitDelay,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req, 100,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrExitDelayBelowSafetyMargin)
	})
}

// TestValidateVTXORequest tests VTXO request validation.
func TestValidateVTXORequest(t *testing.T) {
	t.Parallel()

	clientPub, _ := testutils.CreateKey(2)
	signingKey1, _ := testutils.CreateKey(10)
	signingKey2, _ := testutils.CreateKey(11)

	t.Run("valid VTXO request returns descriptor", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinVTXOAmount = 1000
		h.env.Terms.MaxVTXOAmount = 1000000
		h.env.Terms.VTXOExitDelay = 100

		// Build expected descriptor to get pkScript.
		descriptor, err := tree.NewVTXODescriptor(
			10000, clientPub, h.operatorPub, 144,
		)
		require.NoError(t, err)

		req := &types.VTXORequest{
			Amount: 10000,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, 144,
			),
			PkScript:    descriptor.PkScript,
			Expiry:      144,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			SigningKey: keychain.KeyDescriptor{
				PubKey: signingKey1,
			},
		}

		usedKeys := make(map[SigningKeyHex]*btcec.PublicKey)
		result, err := ValidateVTXORequest(h.env.Terms, req, usedKeys)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, req.Amount, result.Amount)
		require.Equal(t, descriptor.PkScript, result.PkScript)
	})

	t.Run("amount below minimum rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinVTXOAmount = 1000
		h.env.Terms.MaxVTXOAmount = 1000000
		h.env.Terms.VTXOExitDelay = 100

		descriptor, _ := tree.NewVTXODescriptor(
			500, clientPub, h.operatorPub, 144,
		)

		req := &types.VTXORequest{
			Amount: 500,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, 144,
			),
			PkScript:    descriptor.PkScript,
			Expiry:      144,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			SigningKey: keychain.KeyDescriptor{
				PubKey: signingKey1,
			},
		}

		usedKeys := make(map[SigningKeyHex]*btcec.PublicKey)
		result, err := ValidateVTXORequest(h.env.Terms, req, usedKeys)
		require.Nil(t, result)
		require.ErrorIs(t, err, ErrVTXOAmountTooLow)
	})

	t.Run("amount above maximum rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinVTXOAmount = 1000
		h.env.Terms.MaxVTXOAmount = 1000000
		h.env.Terms.VTXOExitDelay = 100

		descriptor, _ := tree.NewVTXODescriptor(
			2000000, clientPub, h.operatorPub, 144,
		)

		req := &types.VTXORequest{
			Amount: 2000000,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, 144,
			),
			PkScript:    descriptor.PkScript,
			Expiry:      144,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			SigningKey: keychain.KeyDescriptor{
				PubKey: signingKey1,
			},
		}

		usedKeys := make(map[SigningKeyHex]*btcec.PublicKey)
		result, err := ValidateVTXORequest(h.env.Terms, req, usedKeys)
		require.Nil(t, result)
		require.ErrorIs(t, err, ErrVTXOAmountTooHigh)
	})

	t.Run("expiry below minimum rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinVTXOAmount = 1000
		h.env.Terms.MaxVTXOAmount = 1000000
		h.env.Terms.VTXOExitDelay = 100

		descriptor, _ := tree.NewVTXODescriptor(
			10000, clientPub, h.operatorPub, 50,
		)

		req := &types.VTXORequest{
			Amount: 10000,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, 50,
			),
			PkScript:    descriptor.PkScript,
			Expiry:      50,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			SigningKey: keychain.KeyDescriptor{
				PubKey: signingKey1,
			},
		}

		usedKeys := make(map[SigningKeyHex]*btcec.PublicKey)
		result, err := ValidateVTXORequest(h.env.Terms, req, usedKeys)
		require.Nil(t, result)
		require.ErrorIs(t, err, ErrVTXOExpiryTooLow)
	})

	t.Run("wrong operator key rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinVTXOAmount = 1000
		h.env.Terms.MaxVTXOAmount = 1000000
		h.env.Terms.VTXOExitDelay = 100

		wrongOpKey, _ := testutils.CreateKey(99)

		descriptor, _ := tree.NewVTXODescriptor(
			10000, clientPub, wrongOpKey, 144,
		)

		req := &types.VTXORequest{
			Amount: 10000,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, wrongOpKey, 144,
			),
			PkScript:    descriptor.PkScript,
			Expiry:      144,
			ClientKey:   clientPub,
			OperatorKey: wrongOpKey,
			SigningKey: keychain.KeyDescriptor{
				PubKey: signingKey1,
			},
		}

		usedKeys := make(map[SigningKeyHex]*btcec.PublicKey)
		result, err := ValidateVTXORequest(h.env.Terms, req, usedKeys)
		require.Nil(t, result)
		require.ErrorIs(t, err, ErrOperatorKeyMismatch)
	})

	t.Run("duplicate signing key rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinVTXOAmount = 1000
		h.env.Terms.MaxVTXOAmount = 1000000
		h.env.Terms.VTXOExitDelay = 100

		descriptor, _ := tree.NewVTXODescriptor(
			10000, clientPub, h.operatorPub, 144,
		)

		req := &types.VTXORequest{
			Amount: 10000,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, 144,
			),
			PkScript:    descriptor.PkScript,
			Expiry:      144,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			SigningKey: keychain.KeyDescriptor{
				PubKey: signingKey1,
			},
		}

		// Mark signingKey1 as already used.
		usedKeys := map[SigningKeyHex]*btcec.PublicKey{
			route.NewVertex(signingKey1): signingKey1,
		}

		result, err := ValidateVTXORequest(h.env.Terms, req, usedKeys)
		require.Nil(t, result)
		require.ErrorIs(t, err, ErrSigningKeyNotUnique)
	})

	t.Run("wrong pkScript rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinVTXOAmount = 1000
		h.env.Terms.MaxVTXOAmount = 1000000
		h.env.Terms.VTXOExitDelay = 100

		req := &types.VTXORequest{
			Amount: 10000,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, 144,
			),
			// Wrong script.
			PkScript:    []byte{0x00, 0x14},
			Expiry:      144,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			SigningKey: keychain.KeyDescriptor{
				PubKey: signingKey1,
			},
		}

		usedKeys := make(map[SigningKeyHex]*btcec.PublicKey)
		result, err := ValidateVTXORequest(h.env.Terms, req, usedKeys)
		require.Nil(t, result)
		require.ErrorIs(t, err, ErrVTXOPkScriptMismatch)
	})

	// An empty req.PkScript was previously silently accepted (the
	// server fell back to its own derivation). That hid any
	// client/server divergence on the policy->pkScript derivation.
	// Require a non-empty pkScript so the belt-and-suspenders check
	// runs on every submission.
	t.Run("missing pkScript rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinVTXOAmount = 1000
		h.env.Terms.MaxVTXOAmount = 1000000
		h.env.Terms.VTXOExitDelay = 100

		req := &types.VTXORequest{
			Amount: 10000,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, 144,
			),
			// Deliberately omit PkScript.
			PkScript:    nil,
			Expiry:      144,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			SigningKey: keychain.KeyDescriptor{
				PubKey: signingKey1,
			},
		}

		usedKeys := make(map[SigningKeyHex]*btcec.PublicKey)
		result, err := ValidateVTXORequest(h.env.Terms, req, usedKeys)
		require.Nil(t, result)
		require.ErrorIs(t, err, ErrVTXOPkScriptMissing)
	})

	t.Run("multiple unique signing keys accepted", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinVTXOAmount = 1000
		h.env.Terms.MaxVTXOAmount = 1000000
		h.env.Terms.VTXOExitDelay = 100

		descriptor1, _ := tree.NewVTXODescriptor(
			10000, clientPub, h.operatorPub, 144,
		)
		descriptor2, _ := tree.NewVTXODescriptor(
			20000, clientPub, h.operatorPub, 144,
		)

		req1 := &types.VTXORequest{
			Amount: 10000,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, 144,
			),
			PkScript:    descriptor1.PkScript,
			Expiry:      144,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			SigningKey: keychain.KeyDescriptor{
				PubKey: signingKey1,
			},
		}

		req2 := &types.VTXORequest{
			Amount: 20000,
			PolicyTemplate: testPolicyTemplate(
				t, clientPub, h.operatorPub, 144,
			),
			PkScript:    descriptor2.PkScript,
			Expiry:      144,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			// Different signing key.
			SigningKey: keychain.KeyDescriptor{
				PubKey: signingKey2,
			},
		}

		usedKeys := make(map[SigningKeyHex]*btcec.PublicKey)

		// First request should succeed.
		result1, err := ValidateVTXORequest(h.env.Terms, req1, usedKeys)
		require.NoError(t, err)
		require.NotNil(t, result1)

		// Track the first signing key.
		key1Vertex := route.NewVertex(signingKey1)
		usedKeys[key1Vertex] = signingKey1

		// Second request with different signing key should succeed.
		result2, err := ValidateVTXORequest(h.env.Terms, req2, usedKeys)
		require.NoError(t, err)
		require.NotNil(t, result2)
	})
}

// TestValidateLeaveRequest tests the ValidateLeaveRequest function.
func TestValidateLeaveRequest(t *testing.T) {
	t.Parallel()

	t.Run("valid leave request succeeds", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinLeaveAmount = 10000

		req := &types.LeaveRequest{
			Output: &wire.TxOut{
				Value:    50000,
				PkScript: []byte{0x00, 0x14, 0x01, 0x02},
			},
		}

		err := ValidateLeaveRequest(h.env.Terms, req)
		require.NoError(t, err)
	})

	t.Run("nil output rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinLeaveAmount = 10000

		req := &types.LeaveRequest{
			Output: nil,
		}

		err := ValidateLeaveRequest(h.env.Terms, req)
		require.ErrorIs(t, err, ErrLeaveOutputNil)
	})

	t.Run("zero value rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinLeaveAmount = 10000

		req := &types.LeaveRequest{
			Output: &wire.TxOut{
				Value:    0,
				PkScript: []byte{0x00, 0x14},
			},
		}

		err := ValidateLeaveRequest(h.env.Terms, req)
		require.ErrorIs(t, err, ErrLeaveOutputValueInvalid)
	})

	t.Run("negative value rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinLeaveAmount = 10000

		req := &types.LeaveRequest{
			Output: &wire.TxOut{
				Value:    -100,
				PkScript: []byte{0x00, 0x14},
			},
		}

		err := ValidateLeaveRequest(h.env.Terms, req)
		require.ErrorIs(t, err, ErrLeaveOutputValueInvalid)
	})

	t.Run("amount below minimum rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinLeaveAmount = 10000

		req := &types.LeaveRequest{
			Output: &wire.TxOut{
				Value:    5000,
				PkScript: []byte{0x00, 0x14, 0x01, 0x02},
			},
		}

		err := ValidateLeaveRequest(h.env.Terms, req)
		require.ErrorIs(t, err, ErrLeaveAmountTooLow)
	})

	t.Run("amount at minimum succeeds", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinLeaveAmount = 10000

		req := &types.LeaveRequest{
			Output: &wire.TxOut{
				Value:    10000,
				PkScript: []byte{0x00, 0x14, 0x01, 0x02},
			},
		}

		err := ValidateLeaveRequest(h.env.Terms, req)
		require.NoError(t, err)
	})

	t.Run("empty pkScript rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinLeaveAmount = 10000

		req := &types.LeaveRequest{
			Output: &wire.TxOut{
				Value:    50000,
				PkScript: []byte{},
			},
		}

		err := ValidateLeaveRequest(h.env.Terms, req)
		require.ErrorIs(t, err, ErrLeaveOutputEmptyPkScript)
	})
}

// TestValidateForfeitRequest tests the ValidateForfeitRequest function.
func TestValidateForfeitRequest(t *testing.T) {
	t.Parallel()

	clientPub, _ := testutils.CreateKey(2)

	// Create a test outpoint for the VTXO.
	vtxoOutpoint := wire.OutPoint{
		Hash:  [32]byte{0x10},
		Index: 0,
	}

	t.Run("valid forfeit request returns input", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Create a live VTXO descriptor.
		descriptor, err := tree.NewVTXODescriptor(
			50000, clientPub, h.operatorPub, 144,
		)
		require.NoError(t, err)

		// Create a VTXO in live status.
		vtxo := &VTXO{
			RoundID:          h.env.RoundID,
			BatchOutputIndex: 0,
			Descriptor:       descriptor,
			Status:           VTXOStatusLive,
		}

		// Set up the VTXO store mock to return the VTXO.
		h.expectVTXO(vtxoOutpoint, vtxo)

		req := &types.ForfeitRequest{
			VTXOOutpoint: &vtxoOutpoint,
		}

		forfeitInput, err := ValidateForfeitRequest(
			t.Context(), h.env, req,
		)
		require.NoError(t, err)
		require.NotNil(t, forfeitInput)
		require.Equal(t, &vtxoOutpoint, forfeitInput.Outpoint)
		require.Equal(t, vtxo, forfeitInput.VTXO)
	})

	t.Run("VTXO not found rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Don't add any VTXO to the store - it should not be found.
		nonExistentOutpoint := wire.OutPoint{
			Hash:  [32]byte{0x99},
			Index: 5,
		}

		// Set up the VTXO store mock to return nil (VTXO not found).
		h.expectVTXO(nonExistentOutpoint, nil)

		req := &types.ForfeitRequest{
			VTXOOutpoint: &nonExistentOutpoint,
		}

		forfeitInput, err := ValidateForfeitRequest(
			t.Context(), h.env, req,
		)
		require.Nil(t, forfeitInput)
		require.ErrorIs(t, err, ErrForfeitVTXONotFound)
	})

	t.Run("VTXO not live (unconfirmed) rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Create an unconfirmed VTXO descriptor.
		descriptor, err := tree.NewVTXODescriptor(
			50000, clientPub, h.operatorPub, 144,
		)
		require.NoError(t, err)

		// Create a VTXO in unconfirmed status.
		vtxo := &VTXO{
			RoundID:          h.env.RoundID,
			BatchOutputIndex: 0,
			Descriptor:       descriptor,
			Status:           VTXOStatusPending,
		}

		// Set up the VTXO store mock to return the VTXO.
		h.expectVTXO(vtxoOutpoint, vtxo)

		req := &types.ForfeitRequest{
			VTXOOutpoint: &vtxoOutpoint,
		}

		forfeitInput, err := ValidateForfeitRequest(
			t.Context(), h.env, req,
		)
		require.Nil(t, forfeitInput)
		require.ErrorIs(t, err, ErrForfeitVTXONotLive)
	})
}

// TestValidateJoinRequest tests the ValidateJoinRequest validation function.
func TestValidateJoinRequest(t *testing.T) {
	t.Parallel()

	clientPub, _ := testutils.CreateKey(2)

	outpoint1 := wire.OutPoint{
		Hash:  [32]byte{0x01},
		Index: 0,
	}

	outpoint2 := wire.OutPoint{
		Hash:  [32]byte{0x02},
		Index: 1,
	}

	t.Run("multiple valid boarding requests", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Set up validation mocks for both outpoints.
		exitDelay := uint32(144)
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, exitDelay, 10,
		)
		h.setupBoardingInputValidationOnly(
			&outpoint2, clientPub, exitDelay, 10,
		)

		req := &types.JoinRoundRequest{
			BoardingReqs: []*types.BoardingRequest{
				{
					Outpoint: &outpoint1,
					PolicyTemplate: testPolicyTemplate(
						t, clientPub, h.operatorPub,
						exitDelay,
					),
					ClientKey:   clientPub,
					OperatorKey: h.operatorPub,
					ExitDelay:   exitDelay,
				},
				{
					Outpoint: &outpoint2,
					PolicyTemplate: testPolicyTemplate(
						t, clientPub, h.operatorPub,
						exitDelay,
					),
					ClientKey:   clientPub,
					OperatorKey: h.operatorPub,
					ExitDelay:   exitDelay,
				},
			},
		}

		result, err := ValidateJoinRequest(t.Context(), h.env, req)

		require.NoError(t, err)
		require.Len(t, result.BoardingInputs, 2)
		require.Equal(t, &outpoint1, result.BoardingInputs[0].Outpoint)
		require.Equal(t, &outpoint2, result.BoardingInputs[1].Outpoint)
	})

	t.Run("duplicate boarding request outpoints", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Set up validation mocks for outpoint1.
		exitDelay := uint32(144)
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, exitDelay, 10,
		)

		// Create join request with duplicate outpoints.
		req := &types.JoinRoundRequest{
			BoardingReqs: []*types.BoardingRequest{
				{
					Outpoint: &outpoint1,
					PolicyTemplate: testPolicyTemplate(
						t, clientPub, h.operatorPub,
						exitDelay,
					),
					ClientKey:   clientPub,
					OperatorKey: h.operatorPub,
					ExitDelay:   exitDelay,
				},
				{
					// Duplicate!
					Outpoint: &outpoint1,
					PolicyTemplate: testPolicyTemplate(
						t, clientPub, h.operatorPub,
						exitDelay,
					),
					ClientKey:   clientPub,
					OperatorKey: h.operatorPub,
					ExitDelay:   exitDelay,
				},
			},
		}

		result, err := ValidateJoinRequest(t.Context(), h.env, req)

		require.Nil(t, result)
		require.ErrorIs(t, err, ErrDuplicateBoardingRequest)
	})

	t.Run("invalid boarding request in join", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Create a RoundID for the lock.
		otherRoundID, err := NewRoundID()
		require.NoError(t, err)

		// Set up: first succeeds, second fails (already locked).
		exitDelay := uint32(144)
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, exitDelay, 10,
		)
		h.lockBoardingInput(&outpoint2, otherRoundID)

		req := &types.JoinRoundRequest{
			BoardingReqs: []*types.BoardingRequest{
				{
					Outpoint: &outpoint1,
					PolicyTemplate: testPolicyTemplate(
						t, clientPub, h.operatorPub,
						exitDelay,
					),
					ClientKey:   clientPub,
					OperatorKey: h.operatorPub,
					ExitDelay:   exitDelay,
				},
				{
					Outpoint: &outpoint2,
					PolicyTemplate: testPolicyTemplate(
						t, clientPub, h.operatorPub,
						exitDelay,
					),
					ClientKey:   clientPub,
					OperatorKey: h.operatorPub,
					ExitDelay:   exitDelay,
				},
			},
		}

		result, err := ValidateJoinRequest(t.Context(), h.env, req)

		require.Nil(t, result)
		require.ErrorIs(t, err, ErrBoardingInputLocked)
	})

	t.Run("valid leave request within balance", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Set up validation mocks for outpoint1 (100000 sats).
		exitDelay := uint32(144)
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, exitDelay, 10,
		)

		// Boarding input is 100000 sats, leave is 50000 sats.
		req := &types.JoinRoundRequest{
			BoardingReqs: []*types.BoardingRequest{{
				Outpoint: &outpoint1,
				PolicyTemplate: testPolicyTemplate(
					t, clientPub, h.operatorPub, exitDelay,
				),
				ClientKey:   clientPub,
				OperatorKey: h.operatorPub,
				ExitDelay:   exitDelay,
			}},
			LeaveReqs: []*types.LeaveRequest{{
				Output: &wire.TxOut{
					Value:    50000,
					PkScript: []byte{0x00, 0x14},
				},
			}},
		}

		result, err := ValidateJoinRequest(t.Context(), h.env, req)

		require.NoError(t, err)
		require.Len(t, result.BoardingInputs, 1)
		require.Len(t, result.RequiredOutputs, 1)
		require.Equal(t, int64(50000), result.RequiredOutputs[0].Value)
	})

	t.Run("leave request exceeds boarding input", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Set up validation mocks for outpoint1 (100000 sats).
		exitDelay := uint32(144)
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, exitDelay, 10,
		)

		// Boarding input is 100000 sats, leave is 150000 sats.
		req := &types.JoinRoundRequest{
			BoardingReqs: []*types.BoardingRequest{{
				Outpoint: &outpoint1,
				PolicyTemplate: testPolicyTemplate(
					t, clientPub, h.operatorPub, exitDelay,
				),
				ClientKey:   clientPub,
				OperatorKey: h.operatorPub,
				ExitDelay:   exitDelay,
			}},
			LeaveReqs: []*types.LeaveRequest{{
				Output: &wire.TxOut{
					Value:    150000,
					PkScript: []byte{0x00, 0x14},
				},
			}},
		}

		result, err := ValidateJoinRequest(t.Context(), h.env, req)

		require.Nil(t, result)
		require.ErrorIs(t, err, ErrOutputExceedsInput)
	})

	t.Run("boarding with VTXO requests", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinVTXOAmount = 1000
		h.env.Terms.MaxVTXOAmount = 1000000
		h.env.Terms.VTXOExitDelay = 100

		// Set up validation mocks for boarding input with 100k sats.
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, 144, 10,
		)

		// Create VTXO request descriptors.
		vtxoKey1, _ := testutils.CreateKey(10)
		vtxoKey2, _ := testutils.CreateKey(11)

		desc1, err := tree.NewVTXODescriptor(
			30000, clientPub, h.operatorPub, 144,
		)
		require.NoError(t, err)

		desc2, err := tree.NewVTXODescriptor(
			40000, clientPub, h.operatorPub, 144,
		)
		require.NoError(t, err)

		req := &types.JoinRoundRequest{
			BoardingReqs: []*types.BoardingRequest{{
				Outpoint: &outpoint1,
				PolicyTemplate: testPolicyTemplate(
					t, clientPub, h.operatorPub, 144,
				),
				ClientKey:   clientPub,
				OperatorKey: h.operatorPub,
				ExitDelay:   144,
			}},
			VTXOReqs: []*types.VTXORequest{
				{
					Amount: 30000,
					PolicyTemplate: testPolicyTemplate(
						t, clientPub, h.operatorPub,
						144,
					),
					PkScript:    desc1.PkScript,
					Expiry:      144,
					ClientKey:   clientPub,
					OperatorKey: h.operatorPub,
					SigningKey: keychain.KeyDescriptor{
						PubKey: vtxoKey1,
					},
				},
				{
					Amount: 40000,
					PolicyTemplate: testPolicyTemplate(
						t, clientPub, h.operatorPub,
						144,
					),
					PkScript:    desc2.PkScript,
					Expiry:      144,
					ClientKey:   clientPub,
					OperatorKey: h.operatorPub,
					SigningKey: keychain.KeyDescriptor{
						PubKey: vtxoKey2,
					},
				},
			},
		}

		result, err := ValidateJoinRequest(t.Context(), h.env, req)

		require.NoError(t, err)
		require.NotNil(t, result)
		require.Len(t, result.BoardingInputs, 1)
		require.Len(t, result.VTXODescriptors, 2)
		require.Len(t, result.SigningKeys, 2)

		// Verify VTXO descriptors and signing keys tracked.
		key1Vertex := route.NewVertex(vtxoKey1)
		key2Vertex := route.NewVertex(vtxoKey2)
		require.Contains(t, result.VTXODescriptors, key1Vertex)
		require.Contains(t, result.VTXODescriptors, key2Vertex)
		require.Equal(t, desc1.PkScript,
			result.VTXODescriptors[key1Vertex].PkScript)
		require.Equal(t, desc2.PkScript,
			result.VTXODescriptors[key2Vertex].PkScript)
		require.Contains(t, result.SigningKeys, key1Vertex)
		require.Contains(t, result.SigningKeys, key2Vertex)
	})

	t.Run("VTXO output exceeds input rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.MinVTXOAmount = 1000
		h.env.Terms.MaxVTXOAmount = 1000000
		h.env.Terms.VTXOExitDelay = 100

		// Set up validation mocks for boarding input with 100k sats.
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, 144, 10,
		)

		vtxoKey, _ := testutils.CreateKey(10)

		// Request VTXO for more than boarding input value.
		desc, err := tree.NewVTXODescriptor(
			150000, clientPub, h.operatorPub, 144,
		)
		require.NoError(t, err)

		req := &types.JoinRoundRequest{
			BoardingReqs: []*types.BoardingRequest{{
				Outpoint: &outpoint1,
				PolicyTemplate: testPolicyTemplate(
					t, clientPub, h.operatorPub, 144,
				),
				ClientKey:   clientPub,
				OperatorKey: h.operatorPub,
				ExitDelay:   144,
			}},
			VTXOReqs: []*types.VTXORequest{{
				Amount: 150000,
				PolicyTemplate: testPolicyTemplate(
					t, clientPub, h.operatorPub, 144,
				),
				PkScript:    desc.PkScript,
				Expiry:      144,
				ClientKey:   clientPub,
				OperatorKey: h.operatorPub,
				SigningKey: keychain.KeyDescriptor{
					PubKey: vtxoKey,
				},
			}},
		}

		result, err := ValidateJoinRequest(t.Context(), h.env, req)

		require.Nil(t, result)
		require.ErrorIs(t, err, ErrOutputExceedsInput)
	})

	t.Run("operator fee below minimum rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Require at least 5000 sats operator fee.
		h.env.Terms.MinOperatorFee = btcutil.Amount(5000)

		// Set up validation mocks for outpoint1 (100000 sats).
		exitDelay := uint32(144)
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, exitDelay, 10,
		)

		// Boarding input is 100000 sats, leave is 99000 sats.
		// Implied fee = 1000 sats, below the 5000 minimum.
		req := &types.JoinRoundRequest{
			BoardingReqs: []*types.BoardingRequest{{
				Outpoint: &outpoint1,
				PolicyTemplate: testPolicyTemplate(
					t, clientPub, h.operatorPub, exitDelay,
				),
				ClientKey:   clientPub,
				OperatorKey: h.operatorPub,
				ExitDelay:   exitDelay,
			}},
			LeaveReqs: []*types.LeaveRequest{{
				Output: &wire.TxOut{
					Value:    99000,
					PkScript: []byte{0x00, 0x14},
				},
			}},
		}

		result, err := ValidateJoinRequest(
			t.Context(), h.env, req,
		)

		require.Nil(t, result)
		require.ErrorIs(t, err, ErrOperatorFeeTooLow)
	})

	t.Run("operator fee exactly at minimum accepted", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Require exactly 5000 sats operator fee.
		h.env.Terms.MinOperatorFee = btcutil.Amount(5000)

		exitDelay := uint32(144)
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, exitDelay, 10,
		)

		// Boarding input is 100000 sats, leave is 95000 sats.
		// Implied fee = 5000 sats, exactly the minimum.
		req := &types.JoinRoundRequest{
			BoardingReqs: []*types.BoardingRequest{{
				Outpoint: &outpoint1,
				PolicyTemplate: testPolicyTemplate(
					t, clientPub, h.operatorPub, exitDelay,
				),
				ClientKey:   clientPub,
				OperatorKey: h.operatorPub,
				ExitDelay:   exitDelay,
			}},
			LeaveReqs: []*types.LeaveRequest{{
				Output: &wire.TxOut{
					Value:    95000,
					PkScript: []byte{0x00, 0x14},
				},
			}},
		}

		result, err := ValidateJoinRequest(
			t.Context(), h.env, req,
		)

		require.NoError(t, err)
		require.Len(t, result.BoardingInputs, 1)
		require.Len(t, result.RequiredOutputs, 1)
	})

	t.Run("zero min fee allows zero-fee join", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// No minimum fee enforcement (default zero value).
		h.env.Terms.MinOperatorFee = 0

		exitDelay := uint32(144)
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, exitDelay, 10,
		)

		// Boarding input is 100000 sats, leave is 100000 sats.
		// Implied fee = 0 sats. Allowed when min is 0.
		req := &types.JoinRoundRequest{
			BoardingReqs: []*types.BoardingRequest{{
				Outpoint: &outpoint1,
				PolicyTemplate: testPolicyTemplate(
					t, clientPub, h.operatorPub, exitDelay,
				),
				ClientKey:   clientPub,
				OperatorKey: h.operatorPub,
				ExitDelay:   exitDelay,
			}},
			LeaveReqs: []*types.LeaveRequest{{
				Output: &wire.TxOut{
					Value:    100000,
					PkScript: []byte{0x00, 0x14},
				},
			}},
		}

		result, err := ValidateJoinRequest(
			t.Context(), h.env, req,
		)

		require.NoError(t, err)
		require.Len(t, result.BoardingInputs, 1)
	})

	t.Run("valid forfeit request with leave output", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Create a live VTXO with 50000 sats.
		vtxoOutpoint := wire.OutPoint{
			Hash:  [32]byte{0x20},
			Index: 0,
		}

		descriptor, err := tree.NewVTXODescriptor(
			50000, clientPub, h.operatorPub, 144,
		)
		require.NoError(t, err)

		vtxo := &VTXO{
			RoundID:          h.env.RoundID,
			BatchOutputIndex: 0,
			Descriptor:       descriptor,
			Status:           VTXOStatusLive,
		}
		h.expectVTXO(vtxoOutpoint, vtxo)

		// Forfeit 50000 sats, leave 30000 sats.
		req := &types.JoinRoundRequest{
			ForfeitReqs: []*types.ForfeitRequest{{
				VTXOOutpoint: &vtxoOutpoint,
			}},
			LeaveReqs: []*types.LeaveRequest{{
				Output: &wire.TxOut{
					Value:    30000,
					PkScript: []byte{0x00, 0x14},
				},
			}},
		}

		result, err := ValidateJoinRequest(t.Context(), h.env, req)

		require.NoError(t, err)
		require.NotNil(t, result)
		require.Len(t, result.ForfeitInputs, 1)
		require.Len(t, result.RequiredOutputs, 1)
		require.Equal(t, &vtxoOutpoint,
			result.ForfeitInputs[0].Outpoint)
		require.Equal(t, int64(30000), result.RequiredOutputs[0].Value)
	})

	t.Run("duplicate forfeit request rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Create a live VTXO.
		vtxoOutpoint := wire.OutPoint{
			Hash:  [32]byte{0x21},
			Index: 0,
		}

		descriptor, err := tree.NewVTXODescriptor(
			50000, clientPub, h.operatorPub, 144,
		)
		require.NoError(t, err)

		vtxo := &VTXO{
			RoundID:          h.env.RoundID,
			BatchOutputIndex: 0,
			Descriptor:       descriptor,
			Status:           VTXOStatusLive,
		}
		h.expectVTXO(vtxoOutpoint, vtxo)

		// Duplicate forfeit request for the same outpoint.
		req := &types.JoinRoundRequest{
			ForfeitReqs: []*types.ForfeitRequest{
				{VTXOOutpoint: &vtxoOutpoint},
				{VTXOOutpoint: &vtxoOutpoint},
			},
		}

		result, err := ValidateJoinRequest(t.Context(), h.env, req)

		require.Nil(t, result)
		require.ErrorIs(t, err, ErrDuplicateForfeitRequest)
	})

	t.Run("forfeit and boarding combined for balance", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Create a live VTXO with 50000 sats.
		vtxoOutpoint := wire.OutPoint{
			Hash:  [32]byte{0x22},
			Index: 0,
		}

		descriptor, err := tree.NewVTXODescriptor(
			50000, clientPub, h.operatorPub, 144,
		)
		require.NoError(t, err)

		vtxo := &VTXO{
			RoundID:          h.env.RoundID,
			BatchOutputIndex: 0,
			Descriptor:       descriptor,
			Status:           VTXOStatusLive,
		}
		h.expectVTXO(vtxoOutpoint, vtxo)

		// Setup a boarding input with 100000 sats.
		h.boardingLocker.On("IsLocked", t.Context(), &outpoint1).
			Return(false, RoundID{}, nil)
		h.mockBoardingUTXO(outpoint1, clientPub, 144, 10)

		// Total input: 50000 (forfeit) + 100000 (boarding) = 150000.
		// Leave output: 120000 sats (less than total).
		req := &types.JoinRoundRequest{
			BoardingReqs: []*types.BoardingRequest{{
				Outpoint: &outpoint1,
				PolicyTemplate: testPolicyTemplate(
					t, clientPub, h.operatorPub, 144,
				),
				ClientKey:   clientPub,
				OperatorKey: h.operatorPub,
				ExitDelay:   144,
			}},
			ForfeitReqs: []*types.ForfeitRequest{{
				VTXOOutpoint: &vtxoOutpoint,
			}},
			LeaveReqs: []*types.LeaveRequest{{
				Output: &wire.TxOut{
					Value:    120000,
					PkScript: []byte{0x00, 0x14},
				},
			}},
		}

		result, err := ValidateJoinRequest(t.Context(), h.env, req)

		require.NoError(t, err)
		require.NotNil(t, result)
		require.Len(t, result.BoardingInputs, 1)
		require.Len(t, result.ForfeitInputs, 1)
		require.Len(t, result.RequiredOutputs, 1)
	})

	t.Run("leave output exceeds forfeit input rejected",
		func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)

			// Create a live VTXO with 50000 sats.
			vtxoOutpoint := wire.OutPoint{
				Hash:  [32]byte{0x23},
				Index: 0,
			}

			descriptor, err := tree.NewVTXODescriptor(
				50000, clientPub, h.operatorPub, 144,
			)
			require.NoError(t, err)

			vtxo := &VTXO{
				RoundID:          h.env.RoundID,
				BatchOutputIndex: 0,
				Descriptor:       descriptor,
				Status:           VTXOStatusLive,
			}
			h.expectVTXO(vtxoOutpoint, vtxo)

			// Forfeit 50000 sats but try to leave 80000 sats.
			req := &types.JoinRoundRequest{
				ForfeitReqs: []*types.ForfeitRequest{{
					VTXOOutpoint: &vtxoOutpoint,
				}},
				LeaveReqs: []*types.LeaveRequest{{
					Output: &wire.TxOut{
						Value:    80000,
						PkScript: []byte{0x00, 0x14},
					},
				}},
			}

			result, err := ValidateJoinRequest(
				t.Context(), h.env, req,
			)

			require.Nil(t, result)
			require.ErrorIs(t, err, ErrOutputExceedsInput)
		})

	t.Run("nil forfeit outpoint rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		req := &types.JoinRoundRequest{
			ForfeitReqs: []*types.ForfeitRequest{{
				VTXOOutpoint: nil,
			}},
		}

		result, err := ValidateJoinRequest(t.Context(), h.env, req)

		require.Nil(t, result)
		require.ErrorIs(t, err, ErrForfeitOutpointNil)
	})
}

// TestValidateForfeitTxs tests the forfeit transaction validation logic.
func TestValidateForfeitTxs(t *testing.T) {
	t.Parallel()

	t.Run("valid forfeit tx passes validation", func(t *testing.T) {
		t.Parallel()

		const (
			vtxoAmount   = btcutil.Amount(50000)
			exitDelay    = 144
			connectorAmt = btcutil.Amount(330)
		)

		clientPriv := testPrivKey(1)
		operatorPriv := testPrivKey(2)

		clientPub := clientPriv.PubKey()
		operatorPub := operatorPriv.PubKey()

		vtxoDesc, err := tree.NewVTXODescriptor(
			vtxoAmount, clientPub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		vtxo := &VTXO{
			Descriptor: vtxoDesc,
			Status:     VTXOStatusLive,
		}

		vtxoOutpoint := wire.OutPoint{
			Hash:  testOutpointHash(t, "vtxo"),
			Index: 0,
		}
		connectorOutpoint := wire.OutPoint{
			Hash:  testOutpointHash(t, "connector"),
			Index: 0,
		}

		connectorScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootOutputKey(operatorPub, nil),
		)
		require.NoError(t, err)

		connectorLeafOutput := &wire.TxOut{
			Value:    int64(connectorAmt),
			PkScript: connectorScript,
		}

		forfeitInput := &ForfeitInput{
			Outpoint: &vtxoOutpoint,
			VTXO:     vtxo,
		}
		reg := &ClientRegistration{
			ForfeitInputs: []*ForfeitInput{forfeitInput},
		}

		forfeitScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootOutputKey(operatorPub, nil),
		)
		require.NoError(t, err)

		forfeitTx := buildForfeitTx(
			t, vtxoOutpoint, vtxoAmount, connectorOutpoint,
			forfeitScript,
		)

		connectorAssignments :=
			map[wire.OutPoint]*ConnectorLeafAssignment{
				vtxoOutpoint: {
					LeafOutpoint: connectorOutpoint,
					LeafOutput:   connectorLeafOutput,
				},
			}

		forfeitSig := forfeitTxSig(
			t, forfeitTx, clientPriv, vtxoOutpoint,
			connectorLeafOutput, operatorPub, exitDelay,
			vtxoDesc,
		)

		err = validateForfeitTxs(
			t.Context(), btclog.Disabled,
			[]*types.ForfeitTxSig{{
				UnsignedTx:    forfeitTx,
				ClientVTXOSig: forfeitSig,
				SpendPath: testStandardForfeitSpendPath(
					t, vtxoDesc, operatorPub, exitDelay,
				),
			}},
			reg, connectorAssignments, forfeitScript,
			operatorPub,
		)
		require.NoError(t, err)
	})

	t.Run("standard forfeit validates owner key even when stored "+
		"cosigner differs", func(t *testing.T) {
		t.Parallel()

		const (
			vtxoAmount   = btcutil.Amount(50000)
			exitDelay    = 144
			connectorAmt = btcutil.Amount(330)
		)

		ownerPriv := testPrivKey(101)
		signingPriv := testPrivKey(102)
		operatorPriv := testPrivKey(103)

		ownerPub := ownerPriv.PubKey()
		signingPub := signingPriv.PubKey()
		operatorPub := operatorPriv.PubKey()

		vtxoDesc, err := tree.NewVTXODescriptor(
			vtxoAmount, ownerPub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		// Simulate the server-side batch tree metadata. It stores the
		// ephemeral round signing key separately from the owner key
		// encoded in the VTXO policy and output script.
		vtxoDesc.CoSignerKey = signingPub

		vtxo := &VTXO{
			Descriptor: vtxoDesc,
			Status:     VTXOStatusLive,
		}

		vtxoOutpoint := wire.OutPoint{
			Hash:  testOutpointHash(t, "owner-vs-signing"),
			Index: 0,
		}
		connectorOutpoint := wire.OutPoint{
			Hash: testOutpointHash(
				t, "owner-vs-signing-connector",
			),
			Index: 0,
		}

		connectorScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootOutputKey(operatorPub, nil),
		)
		require.NoError(t, err)

		connectorLeafOutput := &wire.TxOut{
			Value:    int64(connectorAmt),
			PkScript: connectorScript,
		}

		forfeitInput := &ForfeitInput{
			Outpoint: &vtxoOutpoint,
			VTXO:     vtxo,
		}
		reg := &ClientRegistration{
			ForfeitInputs: []*ForfeitInput{forfeitInput},
		}

		forfeitScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootOutputKey(operatorPub, nil),
		)
		require.NoError(t, err)

		forfeitTx := buildForfeitTx(
			t, vtxoOutpoint, vtxoAmount, connectorOutpoint,
			forfeitScript,
		)

		connectorAssignments :=
			map[wire.OutPoint]*ConnectorLeafAssignment{
				vtxoOutpoint: {
					LeafOutpoint: connectorOutpoint,
					LeafOutput:   connectorLeafOutput,
				},
			}

		forfeitSig := forfeitTxSigForOwner(
			t, forfeitTx, ownerPriv, ownerPub, vtxoOutpoint,
			connectorLeafOutput, operatorPub, exitDelay,
			vtxoDesc,
		)

		err = validateForfeitTxs(
			t.Context(), btclog.Disabled,
			[]*types.ForfeitTxSig{{
				UnsignedTx:    forfeitTx,
				ClientVTXOSig: forfeitSig,
				SpendPath: testStandardForfeitSpendPathForOwner(
					t, ownerPub, operatorPub, exitDelay,
				),
			}},
			reg, connectorAssignments, forfeitScript,
			operatorPub,
		)
		require.NoError(t, err)
	})

	t.Run("missing forfeit assignment rejected", func(t *testing.T) {
		t.Parallel()

		forfeitOutpoint := wire.OutPoint{
			Hash:  testOutpointHash(t, "missing"),
			Index: 0,
		}

		vtxo := &VTXO{
			Descriptor: &tree.VTXODescriptor{
				Amount: 50000,
			},
		}

		reg := &ClientRegistration{
			ForfeitInputs: []*ForfeitInput{
				{Outpoint: &forfeitOutpoint, VTXO: vtxo},
			},
		}

		forfeitTx := buildForfeitTx(
			t, forfeitOutpoint, 50000,
			wire.OutPoint{Hash: testOutpointHash(t, "conn")},
			[]byte{0x51},
		)

		var dummySigBytes [64]byte
		dummySig, err := schnorr.ParseSignature(dummySigBytes[:])
		require.NoError(t, err)

		err = validateForfeitTxs(
			t.Context(), btclog.Disabled,
			[]*types.ForfeitTxSig{{
				UnsignedTx:    forfeitTx,
				ClientVTXOSig: dummySig,
				SpendPath:     testPlaceholderSpendPath(),
			}},
			reg, map[wire.OutPoint]*ConnectorLeafAssignment{},
			[]byte{0x51}, nil,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no connector assignment")
	})

	t.Run("wrong connector leaf rejected", func(t *testing.T) {
		t.Parallel()

		const (
			vtxoAmount   = btcutil.Amount(50000)
			exitDelay    = 144
			connectorAmt = btcutil.Amount(330)
		)

		clientPriv := testPrivKey(3)
		operatorPriv := testPrivKey(4)

		clientPub := clientPriv.PubKey()
		operatorPub := operatorPriv.PubKey()

		vtxoDesc, err := tree.NewVTXODescriptor(
			vtxoAmount, clientPub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		vtxo := &VTXO{
			Descriptor: vtxoDesc,
		}

		vtxoOutpoint := wire.OutPoint{
			Hash:  testOutpointHash(t, "leaf-vtxo"),
			Index: 0,
		}
		expectedConnector := wire.OutPoint{
			Hash:  testOutpointHash(t, "leaf-expected"),
			Index: 0,
		}
		actualConnector := wire.OutPoint{
			Hash:  testOutpointHash(t, "leaf-actual"),
			Index: 1,
		}

		connectorScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootOutputKey(operatorPub, nil),
		)
		require.NoError(t, err)

		connectorLeafOutput := &wire.TxOut{
			Value:    int64(connectorAmt),
			PkScript: connectorScript,
		}

		reg := &ClientRegistration{
			ForfeitInputs: []*ForfeitInput{
				{Outpoint: &vtxoOutpoint, VTXO: vtxo},
			},
		}

		forfeitScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootOutputKey(operatorPub, nil),
		)
		require.NoError(t, err)

		forfeitTx := buildForfeitTx(
			t, vtxoOutpoint, vtxoAmount,
			actualConnector, forfeitScript,
		)

		forfeitSig := forfeitTxSig(
			t, forfeitTx, clientPriv, vtxoOutpoint,
			connectorLeafOutput, operatorPub, exitDelay,
			vtxoDesc,
		)

		connectorAssignments :=
			map[wire.OutPoint]*ConnectorLeafAssignment{
				vtxoOutpoint: {
					LeafOutpoint: expectedConnector,
					LeafOutput:   connectorLeafOutput,
				},
			}

		err = validateForfeitTxs(
			t.Context(), btclog.Disabled,
			[]*types.ForfeitTxSig{{
				UnsignedTx:    forfeitTx,
				ClientVTXOSig: forfeitSig,
				SpendPath: testStandardForfeitSpendPath(
					t, vtxoDesc, operatorPub, exitDelay,
				),
			}},
			reg, connectorAssignments, forfeitScript,
			operatorPub,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(),
			"connector input references wrong")
	})

	t.Run("wrong penalty amount rejected", func(t *testing.T) {
		t.Parallel()

		const (
			vtxoAmount   = btcutil.Amount(50000)
			exitDelay    = 144
			connectorAmt = btcutil.Amount(330)
		)

		clientPriv := testPrivKey(5)
		operatorPriv := testPrivKey(6)

		clientPub := clientPriv.PubKey()
		operatorPub := operatorPriv.PubKey()

		vtxoDesc, err := tree.NewVTXODescriptor(
			vtxoAmount, clientPub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		vtxo := &VTXO{
			Descriptor: vtxoDesc,
		}

		vtxoOutpoint := wire.OutPoint{
			Hash:  testOutpointHash(t, "amount"),
			Index: 0,
		}
		connectorOutpoint := wire.OutPoint{
			Hash:  testOutpointHash(t, "amount-connector"),
			Index: 0,
		}

		connectorScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootOutputKey(operatorPub, nil),
		)
		require.NoError(t, err)

		connectorLeafOutput := &wire.TxOut{
			Value:    int64(connectorAmt),
			PkScript: connectorScript,
		}

		forfeitScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootOutputKey(operatorPub, nil),
		)
		require.NoError(t, err)

		forfeitTx := buildForfeitTx(
			t, vtxoOutpoint, vtxoAmount,
			connectorOutpoint, forfeitScript,
		)
		forfeitTx.TxOut[0].Value = int64(vtxoAmount - 1000)

		forfeitSig := forfeitTxSig(
			t, forfeitTx, clientPriv, vtxoOutpoint,
			connectorLeafOutput, operatorPub, exitDelay,
			vtxoDesc,
		)

		reg := &ClientRegistration{
			ForfeitInputs: []*ForfeitInput{
				{Outpoint: &vtxoOutpoint, VTXO: vtxo},
			},
		}

		connectorAssignments :=
			map[wire.OutPoint]*ConnectorLeafAssignment{
				vtxoOutpoint: {
					LeafOutpoint: connectorOutpoint,
					LeafOutput:   connectorLeafOutput,
				},
			}

		err = validateForfeitTxs(
			t.Context(), btclog.Disabled,
			[]*types.ForfeitTxSig{{
				UnsignedTx:    forfeitTx,
				ClientVTXOSig: forfeitSig,
				SpendPath: testStandardForfeitSpendPath(
					t, vtxoDesc, operatorPub, exitDelay,
				),
			}},
			reg, connectorAssignments, forfeitScript,
			operatorPub,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(),
			"penalty output amount mismatch")
	})

	t.Run("wrong penalty script rejected", func(t *testing.T) {
		t.Parallel()

		const (
			vtxoAmount   = btcutil.Amount(50000)
			exitDelay    = 144
			connectorAmt = btcutil.Amount(330)
		)

		clientPriv := testPrivKey(7)
		operatorPriv := testPrivKey(8)

		clientPub := clientPriv.PubKey()
		operatorPub := operatorPriv.PubKey()

		vtxoDesc, err := tree.NewVTXODescriptor(
			vtxoAmount, clientPub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		vtxo := &VTXO{
			Descriptor: vtxoDesc,
		}

		vtxoOutpoint := wire.OutPoint{
			Hash:  testOutpointHash(t, "script"),
			Index: 0,
		}
		connectorOutpoint := wire.OutPoint{
			Hash:  testOutpointHash(t, "script-connector"),
			Index: 0,
		}

		connectorScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootOutputKey(operatorPub, nil),
		)
		require.NoError(t, err)

		connectorLeafOutput := &wire.TxOut{
			Value:    int64(connectorAmt),
			PkScript: connectorScript,
		}

		correctForfeitScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootOutputKey(operatorPub, nil),
		)
		require.NoError(t, err)

		forfeitTx := buildForfeitTx(
			t, vtxoOutpoint, vtxoAmount,
			connectorOutpoint, correctForfeitScript,
		)
		forfeitTx.TxOut[0].PkScript = []byte("wrong")

		forfeitSig := forfeitTxSig(
			t, forfeitTx, clientPriv, vtxoOutpoint,
			connectorLeafOutput, operatorPub, exitDelay,
			vtxoDesc,
		)

		reg := &ClientRegistration{
			ForfeitInputs: []*ForfeitInput{
				{Outpoint: &vtxoOutpoint, VTXO: vtxo},
			},
		}

		connectorAssignments :=
			map[wire.OutPoint]*ConnectorLeafAssignment{
				vtxoOutpoint: {
					LeafOutpoint: connectorOutpoint,
					LeafOutput:   connectorLeafOutput,
				},
			}

		err = validateForfeitTxs(
			t.Context(), btclog.Disabled,
			[]*types.ForfeitTxSig{{
				UnsignedTx:    forfeitTx,
				ClientVTXOSig: forfeitSig,
				SpendPath: testStandardForfeitSpendPath(
					t, vtxoDesc, operatorPub, exitDelay,
				),
			}},
			reg, connectorAssignments, correctForfeitScript,
			operatorPub,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(),
			"penalty output script does not match")
	})

	t.Run("invalid client signature rejected", func(t *testing.T) {
		t.Parallel()

		const (
			vtxoAmount   = btcutil.Amount(50000)
			exitDelay    = 144
			connectorAmt = btcutil.Amount(330)
		)

		clientPriv := testPrivKey(9)
		operatorPriv := testPrivKey(10)

		clientPub := clientPriv.PubKey()
		operatorPub := operatorPriv.PubKey()

		vtxoDesc, err := tree.NewVTXODescriptor(
			vtxoAmount, clientPub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		vtxo := &VTXO{
			Descriptor: vtxoDesc,
		}

		vtxoOutpoint := wire.OutPoint{
			Hash:  testOutpointHash(t, "sig"),
			Index: 0,
		}
		connectorOutpoint := wire.OutPoint{
			Hash:  testOutpointHash(t, "sig-connector"),
			Index: 0,
		}

		connectorScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootOutputKey(operatorPub, nil),
		)
		require.NoError(t, err)

		connectorLeafOutput := &wire.TxOut{
			Value:    int64(connectorAmt),
			PkScript: connectorScript,
		}

		forfeitScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootOutputKey(operatorPub, nil),
		)
		require.NoError(t, err)

		forfeitTx := buildForfeitTx(
			t, vtxoOutpoint, vtxoAmount,
			connectorOutpoint, forfeitScript,
		)

		badSig := forfeitTxSig(
			t, forfeitTx, operatorPriv, vtxoOutpoint,
			connectorLeafOutput, operatorPub, exitDelay,
			vtxoDesc,
		)

		reg := &ClientRegistration{
			ForfeitInputs: []*ForfeitInput{
				{Outpoint: &vtxoOutpoint, VTXO: vtxo},
			},
		}

		connectorAssignments :=
			map[wire.OutPoint]*ConnectorLeafAssignment{
				vtxoOutpoint: {
					LeafOutpoint: connectorOutpoint,
					LeafOutput:   connectorLeafOutput,
				},
			}

		err = validateForfeitTxs(
			t.Context(), btclog.Disabled,
			[]*types.ForfeitTxSig{{
				UnsignedTx:    forfeitTx,
				ClientVTXOSig: badSig,
				SpendPath: testStandardForfeitSpendPath(
					t, vtxoDesc, operatorPub, exitDelay,
				),
			}},
			reg, connectorAssignments, forfeitScript,
			operatorPub,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid VTXO signature")
	})
}

// TestEnsureForfeitSpendPathCommitsOperatorRejectsNonOperatorLeaf
// asserts that a spend path whose AST leaf does not contain the
// operator key is rejected before any client signature is verified
// or any operator signature is produced. A non-operator-backed leaf
// could previously reach the post-sign script VM as the only
// remaining gate; this closes that window by checking the AST of
// the matched leaf against the configured operator key.
func TestEnsureForfeitSpendPathCommitsOperatorRejectsNonOperatorLeaf(
	t *testing.T) {

	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Build a policy whose only leaf is a client-only Multisig.
	// The compiled script references the client key alone; the AST
	// check must therefore reject the operator-backed requirement.
	leaf := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{clientPriv.PubKey()},
		},
	}
	template := &arkscript.PolicyTemplate{
		Leaves: []arkscript.LeafTemplate{leaf},
	}
	encoded, err := template.Encode()
	require.NoError(t, err)

	compiled, err := template.Compile()
	require.NoError(t, err)

	info, err := compiled.SpendInfo(0)
	require.NoError(t, err)

	vtxo := &VTXO{
		Descriptor: &tree.VTXODescriptor{
			PolicyTemplate: encoded,
		},
	}

	spendPath := &arkscript.SpendPath{
		SpendInfo: info,
	}

	err = ensureForfeitSpendPathCommitsOperator(
		vtxo, spendPath, operatorPriv.PubKey(),
	)
	require.Error(t, err)
	require.Contains(
		t, err.Error(),
		"does not contain operator key",
	)
}

// TestEnsureForfeitSpendPathCommitsOperatorEmptyTemplateNoOp asserts
// that the AST guard is a no-op on OOR-materialised VTXOs that were
// persisted without a policy template. For those rows the post-sign
// script VM remains the final gate; the AST layer has nothing to
// say.
func TestEnsureForfeitSpendPathCommitsOperatorEmptyTemplateNoOp(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	vtxo := &VTXO{
		Descriptor: &tree.VTXODescriptor{
			PolicyTemplate: nil,
		},
	}

	spendPath := &arkscript.SpendPath{
		SpendInfo: &arkscript.SpendInfo{
			WitnessScript: []byte{0x01},
			ControlBlock:  []byte{0x02},
		},
	}

	err = ensureForfeitSpendPathCommitsOperator(
		vtxo, spendPath, operatorPriv.PubKey(),
	)
	require.NoError(t, err)
}

// TestStandardForfeitOwnerKeyCorruptTemplate asserts that a VTXO whose
// persisted PolicyTemplate fails to decode surfaces the decode error
// instead of silently returning (nil, nil) and tricking the caller
// into falling back to CoSignerKey. The previous `return nil, nil`
// on every failure mode collapsed "corrupt template" into "not a
// standard VTXO", which silently downgraded forfeit verification to
// the ephemeral tree signing key for any policy that couldn't round
// trip.
func TestStandardForfeitOwnerKeyCorruptTemplate(t *testing.T) {
	t.Parallel()

	vtxo := &VTXO{
		Descriptor: &tree.VTXODescriptor{
			// Garbage bytes that do not parse as a valid policy
			// template.
			PolicyTemplate: []byte{0xFF, 0xFF, 0xFF, 0xFF},
		},
	}

	spendPath := &arkscript.SpendPath{
		SpendInfo: &arkscript.SpendInfo{
			WitnessScript: []byte{0x01},
			ControlBlock:  []byte{0x02},
		},
	}

	key, err := standardForfeitOwnerKey(vtxo, spendPath)
	require.Error(t, err)
	require.Nil(t, key)
	require.Contains(
		t, err.Error(), "decode persisted policy template",
	)
}

// TestStandardForfeitOwnerKeyEmptyTemplate asserts that an
// OOR-materialised VTXO (no policy template) is still treated as
// legitimately non-standard and returns (nil, nil) rather than an
// error. This preserves the intended fall-through path for rows
// that predate the policy-first model.
func TestStandardForfeitOwnerKeyEmptyTemplate(t *testing.T) {
	t.Parallel()

	vtxo := &VTXO{
		Descriptor: &tree.VTXODescriptor{
			PolicyTemplate: nil,
		},
	}

	spendPath := &arkscript.SpendPath{
		SpendInfo: &arkscript.SpendInfo{
			WitnessScript: []byte{0x01},
			ControlBlock:  []byte{0x02},
		},
	}

	key, err := standardForfeitOwnerKey(vtxo, spendPath)
	require.NoError(t, err)
	require.Nil(t, key)
}

// buildForfeitTx builds a forfeit tx with the specified inputs and script.
func buildForfeitTx(t *testing.T, vtxoOutpoint wire.OutPoint,
	vtxoAmount btcutil.Amount, connectorOutpoint wire.OutPoint,
	forfeitScript []byte) *wire.MsgTx {

	t.Helper()

	forfeitTx, err := tx.BuildForfeitTx(
		&vtxoOutpoint, vtxoAmount, &connectorOutpoint,
		forfeitScript,
	)
	require.NoError(t, err)

	return forfeitTx
}

// forfeitTxSig creates a schnorr signature for a forfeit tx VTXO input.
func forfeitTxSig(t *testing.T, ftx *wire.MsgTx,
	signerPriv *btcec.PrivateKey, vtxoOutpoint wire.OutPoint,
	connectorLeafOutput *wire.TxOut, operatorPub *btcec.PublicKey,
	exitDelay uint32, desc *tree.VTXODescriptor) *schnorr.Signature {

	t.Helper()

	return forfeitTxSigForOwner(
		t, ftx, signerPriv, desc.CoSignerKey, vtxoOutpoint,
		connectorLeafOutput, operatorPub, exitDelay, desc,
	)
}

func forfeitTxSigForOwner(t *testing.T, ftx *wire.MsgTx,
	signerPriv *btcec.PrivateKey, ownerPub *btcec.PublicKey,
	vtxoOutpoint wire.OutPoint, connectorLeafOutput *wire.TxOut,
	operatorPub *btcec.PublicKey, exitDelay uint32,
	desc *tree.VTXODescriptor) *schnorr.Signature {

	t.Helper()

	vtxoTapScript, err := arkscript.VTXOTapScript(
		ownerPub, operatorPub, exitDelay,
	)
	require.NoError(t, err)

	vtxoOutput := &wire.TxOut{
		Value:    int64(desc.Amount),
		PkScript: desc.PkScript,
	}

	connectorOutpoint :=
		ftx.TxIn[tx.ForfeitConnectorInputIndex].PreviousOutPoint

	vtxoCtx := &tx.VTXOSpendContext{
		Outpoint:  vtxoOutpoint,
		Output:    vtxoOutput,
		TapScript: vtxoTapScript,
	}
	connectorCtx := &tx.ConnectorSpendContext{
		Outpoint: connectorOutpoint,
		Output:   connectorLeafOutput,
	}
	prevFetcher, err := tx.NewForfeitPrevOutFetcher(
		vtxoCtx, connectorCtx,
	)
	require.NoError(t, err)

	sigHashes := txscript.NewTxSigHashes(ftx, prevFetcher)

	// Collab path is always at leaf index 0 in the VTXO tapscript.
	const collabLeafIdx = 0
	collabLeaf := vtxoTapScript.Leaves[collabLeafIdx]
	tapLeaf := txscript.NewBaseTapLeaf(collabLeaf.Script)

	sigHash, err := txscript.CalcTapscriptSignaturehash(
		sigHashes, txscript.SigHashDefault, ftx,
		tx.ForfeitVTXOInputIndex, prevFetcher, tapLeaf,
	)
	require.NoError(t, err)

	sig, err := schnorr.Sign(signerPriv, sigHash)
	require.NoError(t, err)

	return sig
}

func testStandardForfeitSpendPath(t *testing.T, desc *tree.VTXODescriptor,
	operatorPub *btcec.PublicKey, exitDelay uint32) *arkscript.SpendPath {

	t.Helper()

	return testStandardForfeitSpendPathForOwner(
		t, desc.CoSignerKey, operatorPub, exitDelay,
	)
}

func testStandardForfeitSpendPathForOwner(t *testing.T,
	ownerPub *btcec.PublicKey, operatorPub *btcec.PublicKey,
	exitDelay uint32) *arkscript.SpendPath {

	t.Helper()

	policy, err := arkscript.NewVTXOPolicy(
		ownerPub, operatorPub, exitDelay,
	)
	require.NoError(t, err)

	spendInfo, err := policy.CollabSpendInfo()
	require.NoError(t, err)

	return &arkscript.SpendPath{
		SpendInfo: spendInfo,
	}
}

func testPlaceholderSpendPath() *arkscript.SpendPath {
	return &arkscript.SpendPath{
		SpendInfo: &arkscript.SpendInfo{
			WitnessScript: []byte{txscript.OP_TRUE},
			ControlBlock:  bytes.Repeat([]byte{0x01}, 33),
		},
	}
}

// testPrivKey returns a deterministic private key for tests.
func testPrivKey(index byte) *btcec.PrivateKey {
	keyBytes := make([]byte, 32)
	keyBytes[31] = index

	privKey, _ := btcec.PrivKeyFromBytes(keyBytes)

	return privKey
}

// testOutpointHash returns a deterministic hash for outpoints in tests.
func testOutpointHash(t *testing.T, tag string) chainhash.Hash {
	t.Helper()

	return chainhash.HashH([]byte(tag))
}

// TestValidateOperatorFee covers the fee-admission logic directly so
// the per-input scaling behavior is locked down against regressions.
func TestValidateOperatorFee(t *testing.T) {
	t.Parallel()

	// A modest fee rate and tree size produce a small but non-zero
	// per-input on-chain share. BaseMarginSat adds a fixed per-input
	// charge so the total expected fee is trivial to reason about.
	const (
		batchSize  = 8
		margin     = 100
		feeRateKW  = chainfee.SatPerKWeight(1000)
		confTarget = 6
	)

	newEnv := func(t *testing.T,
		policy fees.DustPolicy) *Environment {

		t.Helper()

		sched := &fees.Schedule{
			AnnualRate:          0.0,
			BaseMarginSat:       margin,
			MinViableVTXOPolicy: policy,
			MinViableVTXOPct:    50,
		}
		calc, err := fees.NewCalculator(sched)
		require.NoError(t, err)

		mockEstimator := &chainfee.MockEstimator{}
		mockEstimator.On("EstimateFeePerKW", uint32(confTarget)).
			Return(feeRateKW, nil).Maybe()

		return &Environment{
			Terms: &batch.Terms{
				MaxVTXOsPerTree: batchSize,
				MinOperatorFee:  btcutil.Amount(123),
			},
			FeeEstimator:  mockEstimator,
			FeeCalculator: calc,
			ConfTarget:    confTarget,
		}
	}

	// Per-input expected fee for a batch of `batchSize` at
	// `feeRateKW`. Ask the calculator directly rather than
	// reimplementing its ceiling arithmetic so the expected
	// value tracks the code under test.
	refCalc, err := fees.NewCalculator(&fees.Schedule{
		BaseMarginSat:       margin,
		MinViableVTXOPolicy: fees.DustPolicyReject,
		MinViableVTXOPct:    50,
	})
	require.NoError(t, err)
	perInputFee := refCalc.ComputeBoardingFee(
		1_000_000, batchSize, feeRateKW,
	).TotalFeeSat

	makeInputs := func(amounts ...btcutil.Amount) []*BoardingInput {
		out := make([]*BoardingInput, 0, len(amounts))
		for _, a := range amounts {
			out = append(out, &BoardingInput{Value: a})
		}

		return out
	}

	t.Run("fallback flat fee rejects below minimum", func(t *testing.T) {
		t.Parallel()

		env := &Environment{
			Terms: &batch.Terms{MinOperatorFee: 5000},
		}

		err := validateOperatorFee(
			env, 4999, makeInputs(100000), nil,
			batchSize, 0,
		)
		require.ErrorIs(t, err, ErrOperatorFeeTooLow)

		err = validateOperatorFee(
			env, 5000, makeInputs(100000), nil,
			batchSize, 0,
		)
		require.NoError(t, err)
	})

	t.Run("dynamic single input at required fee", func(t *testing.T) {
		t.Parallel()

		env := newEnv(t, fees.DustPolicyReject)
		err := validateOperatorFee(
			env, btcutil.Amount(perInputFee),
			makeInputs(1_000_000), nil,
			batchSize, 0,
		)
		require.NoError(t, err)
	})

	t.Run("dynamic single input below required fee", func(t *testing.T) {
		t.Parallel()

		env := newEnv(t, fees.DustPolicyReject)
		err := validateOperatorFee(
			env, btcutil.Amount(perInputFee-1),
			makeInputs(1_000_000), nil,
			batchSize, 0,
		)
		require.ErrorIs(t, err, ErrOperatorFeeTooLow)
	})

	t.Run("dynamic multi input requires aggregate fee", func(t *testing.T) {
		t.Parallel()

		env := newEnv(t, fees.DustPolicyReject)
		inputs := makeInputs(
			1_000_000, 1_000_000, 1_000_000,
		)

		// Paying only one input's worth for three inputs must
		// fail. Before the scaling fix, this passed.
		err := validateOperatorFee(
			env, btcutil.Amount(perInputFee), inputs, nil,
			batchSize, 0,
		)
		require.ErrorIs(t, err, ErrOperatorFeeTooLow)

		// Paying exactly N * perInputFee across N inputs is
		// accepted.
		err = validateOperatorFee(
			env, btcutil.Amount(perInputFee*3), inputs, nil,
			batchSize, 0,
		)
		require.NoError(t, err)
	})

	t.Run("dust input siblings reject policy", func(t *testing.T) {
		t.Parallel()

		env := newEnv(t, fees.DustPolicyReject)

		// A 200-sat input cannot viably absorb a per-input fee
		// in the low hundreds (> 50% of value), even though
		// the aggregate operator fee covers the round.
		inputs := makeInputs(
			1_000_000, btcutil.Amount(200),
		)
		err := validateOperatorFee(
			env, btcutil.Amount(perInputFee*2), inputs, nil,
			batchSize, 0,
		)
		require.ErrorIs(t, err, ErrVTXOBelowMinViable)
	})

	t.Run("dust input tolerated under warn policy", func(t *testing.T) {
		t.Parallel()

		env := newEnv(t, fees.DustPolicyWarn)
		inputs := makeInputs(
			1_000_000, btcutil.Amount(200),
		)
		err := validateOperatorFee(
			env, btcutil.Amount(perInputFee*2), inputs, nil,
			batchSize, 0,
		)
		require.NoError(t, err)
	})
}
