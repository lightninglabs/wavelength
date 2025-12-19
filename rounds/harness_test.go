package rounds

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// fsmTestHarness is the central test harness housing all common setup,
// mocks, fixtures, and helper functions for round FSM tests.
type fsmTestHarness struct {
	*testing.T

	// Keys and signers for test identities.
	operatorPub    *btcec.PublicKey
	operatorSigner input.Signer

	// Mocks (testify/mock.Mock based).
	boardingLocker *mockBoardingInputLocker
	chainSource    *mockChainSource

	// Environment for FSM.
	env *Environment

	// fsm is the state machine instance under test.
	fsm *StateMachine

	// outboxMessages accumulates outbox events from the last sendEvent
	// call.
	outboxMessages []OutboxEvent
}

// newTestHarness creates a new test harness with default configuration.
// It initializes and starts a new state machine for testing.
func newTestHarness(t *testing.T) *fsmTestHarness {
	t.Helper()

	roundID, err := NewRoundID()
	require.NoError(t, err)

	// Generate deterministic test keys.
	operatorPub, operatorSigner := testutils.CreateKey(1)
	operatorKey := keychain.KeyDescriptor{
		PubKey: operatorPub,
	}

	// Create mocks.
	mockLocker := &mockBoardingInputLocker{}
	mockChainSrc := &mockChainSource{}

	env := Environment{
		RoundID:             roundID,
		ChainParams:         &chaincfg.RegressionNetParams,
		BoardingInputLocker: mockLocker,
		ChainSource:         mockChainSrc,
		Terms: &batch.Terms{
			OperatorKey:                   operatorKey,
			BoardingExitDelay:             100,
			BoardingExitDelaySafetyMargin: 6,
			MinBoardingConfirmations:      1,
			MaxVTXOsPerTree:               1024,
			TreeRadix:                     4,
		},
	}

	fsmCfg := StateMachineCfg{
		InitialState: &CreatedState{},
		Env:          &env,
		Logger:       btclog.Disabled,
	}
	fsm := protofsm.NewStateMachine(fsmCfg)
	fsm.Start(t.Context())

	h := &fsmTestHarness{
		T:              t,
		env:            &env,
		fsm:            &fsm,
		operatorPub:    operatorPub,
		operatorSigner: operatorSigner,
		boardingLocker: mockLocker,
		chainSource:    mockChainSrc,
		outboxMessages: make([]OutboxEvent, 0),
	}

	return h
}

// mockBoardingUTXO sets up a ChainSource mock for a boarding UTXO with the
// specified parameters.
func (h *fsmTestHarness) mockBoardingUTXO(outpoint wire.OutPoint,
	clientKey *btcec.PublicKey, exitDelay uint32, confirmations int64) {

	h.Helper()

	expectedPkScript := buildExpectedPkScript(
		h.T, clientKey, h.operatorPub, exitDelay,
	)

	utxo := &UTXO{
		Output: &wire.TxOut{
			Value:    100000,
			PkScript: expectedPkScript,
		},
		Confirmations: confirmations,
	}
	h.chainSource.On("GetUTXO", outpoint).Return(utxo, nil)
}

// buildExpectedPkScript builds the expected PkScript for a boarding input.
func buildExpectedPkScript(t *testing.T, clientKey *btcec.PublicKey,
	operatorKey *btcec.PublicKey, exitDelay uint32) []byte {

	t.Helper()

	// Build the expected tapscript using the scripts package.
	tapscript, err := scripts.VTXOTapScript(
		clientKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	// Build the P2TR script from the tapscript.
	outputKey, err := tapscript.TaprootKey()
	require.NoError(t, err)

	pkScript, err := input.PayToTaprootScript(outputKey)
	require.NoError(t, err)

	return pkScript
}

// sendEvent sends an event to the state machine and accumulates outbox
// messages. The state machine executor automatically handles dispatching
// internal events, so this method simply awaits the result and captures
// the accumulated outbox events.
//
//nolint:unused
func (h *fsmTestHarness) sendEvent(event Event) error {
	h.Helper()

	// Use AskEvent to send the event and wait for all state transitions
	// (including those triggered by internal events) to complete.
	future := h.fsm.AskEvent(h.Context(), event)
	result := future.Await(h.Context())

	// Extract outbox events or return the error.
	outbox, err := result.Unpack()
	if err != nil {
		return err
	}

	// Accumulate the outbox messages from this event.
	h.outboxMessages = append(h.outboxMessages, outbox...)

	return nil
}

// clearOutbox clears the captured outbox messages. Useful between multiple
// event sends when testing specific sequences.
//
//nolint:unused
func (h *fsmTestHarness) clearOutbox() {
	h.outboxMessages = nil
}

// assertStateType asserts the current state is of the expected type and
// returns it cast to that type.
func assertStateType[T State](h *fsmTestHarness) T {
	h.Helper()

	currentState, err := h.fsm.CurrentState()
	require.NoError(h, err, "failed to query current state")

	state, ok := currentState.(T)
	require.True(h, ok, "current state is not of expected type %T, got "+
		"%T", *new(T), currentState)

	return state
}

// assertOutboxLen asserts that exactly n outbox messages were emitted.
func (h *fsmTestHarness) assertOutboxLen(n int) {
	h.Helper()

	require.Len(h, h.outboxMessages, n)
}

// assertOutboxMessageType asserts that the outbox contains a message of the
// given type at the specified index and returns it cast to that type.
//
//nolint:unused
func assertOutboxMessageType[T OutboxEvent](h *fsmTestHarness,
	index int) T {

	h.Helper()

	require.Greater(h, len(h.outboxMessages), index)

	msg, ok := h.outboxMessages[index].(T)
	require.True(h, ok)

	return msg
}

// mockBoardingInputLocker is a mock implementation of BoardingInputLocker for
// testing using testify/mock.
type mockBoardingInputLocker struct {
	mock.Mock
}

// Lock is a mock implementation of BoardingInputLocker.Lock.
func (m *mockBoardingInputLocker) Lock(ctx context.Context,
	outpoint *wire.OutPoint, roundID RoundID) error {

	args := m.Called(ctx, outpoint, roundID)

	return args.Error(0)
}

// Unlock is a mock implementation of BoardingInputLocker.Unlock.
func (m *mockBoardingInputLocker) Unlock(ctx context.Context,
	outpoint *wire.OutPoint, roundID RoundID) error {

	args := m.Called(ctx, outpoint, roundID)

	return args.Error(0)
}

// IsLocked is a mock implementation of BoardingInputLocker.IsLocked.
func (m *mockBoardingInputLocker) IsLocked(ctx context.Context,
	outpoint *wire.OutPoint) (bool, RoundID, error) {

	args := m.Called(ctx, outpoint)

	var roundID RoundID
	if args.Get(1) != nil {
		roundID = args.Get(1).(RoundID) //nolint:forcetypeassert
	}

	return args.Bool(0), roundID, args.Error(2)
}

// mockChainSource mocks the ChainSource interface for testing.
type mockChainSource struct {
	mock.Mock
}

// GetUTXO is a mock implementation of ChainSource.GetUTXO.
func (m *mockChainSource) GetUTXO(outpoint wire.OutPoint) (*UTXO, error) {
	args := m.Called(outpoint)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*UTXO), args.Error(1) //nolint:forcetypeassert
}
