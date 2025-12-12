package rounds

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

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

		// Mock: no locks on this input.
		h.boardingLocker.On("IsLocked", t.Context(), &outpoint1).
			Return(false, RoundID{}, nil)

		// Mock ChainSource to return valid UTXO with script.
		exitDelay := uint32(144)
		h.mockBoardingUTXO(outpoint1, clientPub, exitDelay, 10)

		req := &types.BoardingRequest{
			Outpoint:    &outpoint1,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   exitDelay,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req,
		)
		require.NoError(t, err)

		// Verify returned BoardingInput has expected values.
		require.NotNil(t, boardingInput)
		require.Equal(t, &outpoint1, boardingInput.Outpoint)
		require.Equal(t, req.ClientKey, boardingInput.ClientKey)
		require.NotNil(t, boardingInput.Tapscript)
		require.NotNil(t, boardingInput.PkScript)
		require.NotNil(t, boardingInput.OperatorKeyDesc)

		h.boardingLocker.AssertExpectations(t)
		h.chainSource.AssertExpectations(t)
	})

	t.Run("boarding input already locked", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Create a RoundID for the lock.
		otherRoundID, err := NewRoundID()
		require.NoError(t, err)

		// Mock: input is already locked by another round.
		h.boardingLocker.On("IsLocked", t.Context(), &outpoint1).
			Return(true, otherRoundID, nil)

		req := &types.BoardingRequest{
			Outpoint:    &outpoint1,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   144,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrBoardingInputLocked)

		h.boardingLocker.AssertExpectations(t)
	})

	t.Run("is locked check fails", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Mock: IsLocked returns an error.
		h.boardingLocker.On("IsLocked", t.Context(), &outpoint1).
			Return(false, RoundID{}, fmt.Errorf("database error"))

		req := &types.BoardingRequest{
			Outpoint:    &outpoint1,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   144,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrCheckLockFailed)

		h.boardingLocker.AssertExpectations(t)
	})

	t.Run("operator key mismatch", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Mock: no locks on this input.
		h.boardingLocker.On("IsLocked", t.Context(), &outpoint1).
			Return(false, RoundID{}, nil)

		req := &types.BoardingRequest{
			Outpoint:  &outpoint1,
			ClientKey: clientPub,
			// Wrong operator key.
			OperatorKey: wrongOpPub,
			ExitDelay:   144,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrOperatorKeyMismatch)

		h.boardingLocker.AssertExpectations(t)
	})

	t.Run("exit delay below minimum", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Minimum exit delay of 200 blocks.
		h.env.Terms.BoardingExitDelay = 200

		// Mock: no locks.
		h.boardingLocker.On("IsLocked", t.Context(), &outpoint1).
			Return(false, RoundID{}, nil)

		req := &types.BoardingRequest{
			Outpoint:    &outpoint1,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			// Less than minimum.
			ExitDelay: 100,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrExitDelayTooLow)

		h.boardingLocker.AssertExpectations(t)
	})

	t.Run("utxo not found or spent", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Mock: no locks.
		h.boardingLocker.On("IsLocked", t.Context(), &outpoint1).
			Return(false, RoundID{}, nil)

		// Mock ChainSource to return error (UTXO doesn't exist).
		h.chainSource.On("GetUTXO", outpoint1).
			Return(nil, fmt.Errorf("utxo not found"))

		req := &types.BoardingRequest{
			Outpoint:    &outpoint1,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   144,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrFetchUTXO)

		h.boardingLocker.AssertExpectations(t)
	})

	t.Run("utxo confirmations below minimum", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.env.Terms.BoardingExitDelay = 100
		h.env.Terms.MinBoardingConfirmations = 10

		// Mock: no locks.
		h.boardingLocker.On("IsLocked", t.Context(), &outpoint1).
			Return(false, RoundID{}, nil)

		// Mock ChainSource to return UTXO with few confirmations.
		exitDelay := uint32(144)
		h.mockBoardingUTXO(outpoint1, clientPub, exitDelay, 5)

		req := &types.BoardingRequest{
			Outpoint:    &outpoint1,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   exitDelay,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrInsufficientConfirmations)

		h.boardingLocker.AssertExpectations(t)
	})

	t.Run("pkscript mismatch", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Mock: no locks.
		h.boardingLocker.On("IsLocked", t.Context(), &outpoint1).
			Return(false, RoundID{}, nil)

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
			Outpoint:    &outpoint1,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   144,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrPkScriptMismatch)

		h.boardingLocker.AssertExpectations(t)
		h.chainSource.AssertExpectations(t)
	})

	t.Run("delay path too close - at safety margin", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Mock: no locks.
		h.boardingLocker.On("IsLocked", t.Context(), &outpoint1).
			Return(false, RoundID{}, nil)

		// Exit delay is 100, safety margin is 6, so max safe is 94.
		// Set confirmations to exactly 94 (at the boundary).
		exitDelay := uint32(100)
		safetyMargin := h.env.Terms.BoardingExitDelaySafetyMargin
		confirmations := int64(exitDelay - safetyMargin)
		h.mockBoardingUTXO(
			outpoint1, clientPub, exitDelay, confirmations,
		)

		req := &types.BoardingRequest{
			Outpoint:    &outpoint1,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   exitDelay,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrDelayPathTooClose)

		h.boardingLocker.AssertExpectations(t)
		h.chainSource.AssertExpectations(t)
	})

	t.Run("delay path too close - past safety margin", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Mock: no locks.
		h.boardingLocker.On("IsLocked", t.Context(), &outpoint1).
			Return(false, RoundID{}, nil)

		// Exit delay is 100, set confirmations to 98 (well past
		// safety margin).
		exitDelay := uint32(100)
		confirmations := int64(98)
		h.mockBoardingUTXO(
			outpoint1, clientPub, exitDelay, confirmations,
		)

		req := &types.BoardingRequest{
			Outpoint:    &outpoint1,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   exitDelay,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req,
		)
		require.Nil(t, boardingInput)
		require.ErrorIs(t, err, ErrDelayPathTooClose)

		h.boardingLocker.AssertExpectations(t)
		h.chainSource.AssertExpectations(t)
	})

	t.Run("valid confirmations within safety margin", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Mock: no locks.
		h.boardingLocker.On("IsLocked", t.Context(), &outpoint1).
			Return(false, RoundID{}, nil)

		// Exit delay is 100, safety margin is 6, so max safe is 94.
		// Set confirmations to 93 (just below the threshold).
		exitDelay := uint32(100)
		confirmations := int64(93)
		h.mockBoardingUTXO(
			outpoint1, clientPub, exitDelay, confirmations,
		)

		req := &types.BoardingRequest{
			Outpoint:    &outpoint1,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			ExitDelay:   exitDelay,
		}

		boardingInput, err := ValidateBoardingRequest(
			t.Context(), h.env, req,
		)
		require.NoError(t, err)
		require.NotNil(t, boardingInput)

		h.boardingLocker.AssertExpectations(t)
		h.chainSource.AssertExpectations(t)
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
			Amount:      10000,
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
			Amount:      500,
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
			Amount:      2000000,
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
			Amount:      10000,
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
			Amount:      10000,
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
			Amount:      10000,
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
			Amount:      10000,
			PkScript:    descriptor1.PkScript,
			Expiry:      144,
			ClientKey:   clientPub,
			OperatorKey: h.operatorPub,
			SigningKey: keychain.KeyDescriptor{
				PubKey: signingKey1,
			},
		}

		req2 := &types.VTXORequest{
			Amount:      20000,
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
