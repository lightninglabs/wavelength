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

		// Set up validation mocks.
		exitDelay := uint32(144)
		h.setupBoardingInputValidationOnly(
			&outpoint1, clientPub, exitDelay, 10,
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
	})

	t.Run("operator key mismatch", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Mock: no locks on this input.
		h.allowBoardingInput(&outpoint1)

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
	})

	t.Run("exit delay below minimum", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Minimum exit delay of 200 blocks.
		h.env.Terms.BoardingExitDelay = 200

		// Mock: no locks.
		h.allowBoardingInput(&outpoint1)

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
			Status:           VTXOStatusUnconfirmed,
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
					Outpoint:    &outpoint1,
					ClientKey:   clientPub,
					OperatorKey: h.operatorPub,
					ExitDelay:   exitDelay,
				},
				{
					Outpoint:    &outpoint2,
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
					Outpoint:    &outpoint1,
					ClientKey:   clientPub,
					OperatorKey: h.operatorPub,
					ExitDelay:   exitDelay,
				},
				{
					// Duplicate!
					Outpoint:    &outpoint1,
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
					Outpoint:    &outpoint1,
					ClientKey:   clientPub,
					OperatorKey: h.operatorPub,
					ExitDelay:   exitDelay,
				},
				{
					Outpoint:    &outpoint2,
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
				Outpoint:    &outpoint1,
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
				Outpoint:    &outpoint1,
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
				Outpoint:    &outpoint1,
				ClientKey:   clientPub,
				OperatorKey: h.operatorPub,
				ExitDelay:   144,
			}},
			VTXOReqs: []*types.VTXORequest{
				{
					Amount:      30000,
					PkScript:    desc1.PkScript,
					Expiry:      144,
					ClientKey:   clientPub,
					OperatorKey: h.operatorPub,
					SigningKey: keychain.KeyDescriptor{
						PubKey: vtxoKey1,
					},
				},
				{
					Amount:      40000,
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
				Outpoint:    &outpoint1,
				ClientKey:   clientPub,
				OperatorKey: h.operatorPub,
				ExitDelay:   144,
			}},
			VTXOReqs: []*types.VTXORequest{{
				Amount:      150000,
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
		require.Equal(t, &vtxoOutpoint, result.ForfeitInputs[0].Outpoint)
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
				Outpoint:    &outpoint1,
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

	t.Run("leave output exceeds forfeit input rejected", func(t *testing.T) {
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

		result, err := ValidateJoinRequest(t.Context(), h.env, req)

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
