package rounds

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightninglabs/darepo/timeout"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// mockClientConnRef implements actor.TellOnlyRef[clientconn.ClientConnMsg]
// and captures all messages sent to clients.
type mockClientConnRef struct {
	t        *testing.T
	id       string
	messages []clientconn.ClientConnMsg
	mu       sync.Mutex
}

// newMockClientConnRef creates a new mock client connection reference.
func newMockClientConnRef(t *testing.T) *mockClientConnRef {
	return &mockClientConnRef{
		t:        t,
		id:       "mock-clients-conn",
		messages: make([]clientconn.ClientConnMsg, 0),
	}
}

// ID returns the ID of this mock actor reference.
func (m *mockClientConnRef) ID() string {
	return m.id
}

// Tell captures a message sent to clients.
func (m *mockClientConnRef) Tell(_ context.Context,
	msg clientconn.ClientConnMsg) {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = append(m.messages, msg)
}

// getMessages returns a copy of all captured messages.
func (m *mockClientConnRef) getMessages() []clientconn.ClientConnMsg {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]clientconn.ClientConnMsg{}, m.messages...)
}

// clearMessages clears all captured messages.
//
//nolint:unused
func (m *mockClientConnRef) clearMessages() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = nil
}

// actorRef wraps an Actor and implements actor.TellOnlyRef[ActorMsg] so that
// the actor can receive asynchronous notifications during tests.
type actorRef struct {
	actor *Actor
}

// ID returns the ID of this actor reference.
func (r *actorRef) ID() string {
	return "rounds-actor-ref"
}

// Tell sends a message to the wrapped actor by calling Receive.
func (r *actorRef) Tell(ctx context.Context, msg ActorMsg) {
	_ = r.actor.Receive(ctx, msg)
}

// baseActorRefMarker implements the BaseActorRef sealed interface marker.
//
//nolint:unused
func (r *actorRef) baseActorRefMarker() {}

// mockTimeoutActor implements actor.TellOnlyRef[timeout.Msg] and captures all
// timeout schedule/cancel calls for testing. In tests, call FireTimeout() to
// simulate a timeout expiring.
type mockTimeoutActor struct {
	t            *testing.T
	mu           sync.Mutex
	scheduledIDs map[timeout.ID]time.Duration
	cancelledIDs []timeout.ID

	// callbacks stores the callback refs provided with each schedule
	// request.
	callbacks map[timeout.ID]actor.TellOnlyRef[*timeout.ExpiredMsg]
}

// newMockTimeoutActor creates a new mock timeout actor.
func newMockTimeoutActor(t *testing.T) *mockTimeoutActor {
	return &mockTimeoutActor{
		t:            t,
		scheduledIDs: make(map[timeout.ID]time.Duration),
		callbacks: make(
			map[timeout.ID]actor.TellOnlyRef[*timeout.ExpiredMsg],
		),
	}
}

// ID returns the ID of this mock actor.
func (m *mockTimeoutActor) ID() string {
	return "mock-timeout-actor"
}

// Tell implements actor.TellOnlyRef[timeout.Msg].
func (m *mockTimeoutActor) Tell(_ context.Context, msg timeout.Msg) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch req := msg.(type) {
	case *timeout.ScheduleTimeoutRequest:
		m.scheduledIDs[req.ID] = req.Duration
		m.callbacks[req.ID] = req.Callback

	case *timeout.CancelTimeoutRequest:
		delete(m.scheduledIDs, req.ID)
		delete(m.callbacks, req.ID)
		m.cancelledIDs = append(m.cancelledIDs, req.ID)
	}
}

// FireTimeout simulates a timeout expiring for the given round ID and phase by
// constructing the composite timeout ID and sending an ExpiredMsg to the
// callback that was provided when the timeout was scheduled. After firing, the
// timeout is removed from scheduledIDs since it's no longer pending.
func (m *mockTimeoutActor) FireTimeout(ctx context.Context, roundID RoundID,
	phase TimeoutPhase) {

	id := makeTimeoutID(roundID, phase)

	m.mu.Lock()
	callback := m.callbacks[id]

	// Remove from scheduled since it's now firing.
	delete(m.scheduledIDs, id)
	delete(m.callbacks, id)
	m.mu.Unlock()

	if callback == nil {
		m.t.Fatalf("no callback registered for timeout ID %s", id)
	}

	callback.Tell(ctx, &timeout.ExpiredMsg{
		ID: id,
	})
}

// assertTimeoutScheduled verifies that a timeout was scheduled for the given
// round ID and phase.
func (m *mockTimeoutActor) assertTimeoutScheduled(t *testing.T, roundID RoundID,
	phase TimeoutPhase) {

	t.Helper()

	id := makeTimeoutID(roundID, phase)

	m.mu.Lock()
	defer m.mu.Unlock()

	_, ok := m.scheduledIDs[id]
	require.True(t, ok, "expected timeout scheduled for ID %s", id)
}

// actorTestHarness provides test infrastructure for the rounds Actor.
type actorTestHarness struct {
	t *testing.T

	//nolint:containedctx
	ctx              context.Context
	actor            *Actor
	cfg              *ActorConfig
	locker           *mockBoardingInputLocker
	clients          *mockClientConnRef
	chainSource      *mockChainSource
	feeEstimator     *chainfee.MockEstimator
	walletController *mockWalletController
	timeoutActor     *mockTimeoutActor

	// operatorPub is the operator public key for this test harness.
	operatorPub *btcec.PublicKey
}

// newActorTestHarness creates a new actor test harness with default
// configuration.
func newActorTestHarness(t *testing.T) *actorTestHarness {
	t.Helper()

	ctx := t.Context()
	locker := &mockBoardingInputLocker{}
	clients := newMockClientConnRef(t)
	chainSource := &mockChainSource{}
	timeoutActor := newMockTimeoutActor(t)

	// Set up fee estimator to return 1000 sat/kw for conf target 6.
	mockFeeEstimator := &chainfee.MockEstimator{}

	// Set up operator key.
	operatorPub, operatorSigner := testutils.CreateKey(1)

	// Set up wallet controller to return success for FundPsbt.
	// Returns change index -1 (no change output).
	mockWalletController := newMockWalletController(operatorSigner)

	cfg := &ActorConfig{
		ChainParams:         &chaincfg.RegressionNetParams,
		Logger:              btclog.Disabled,
		ClientsConn:         clients,
		BoardingInputLocker: locker,
		ChainSource:         chainSource,
		TimeoutActor:        timeoutActor,
		FeeEstimator:        mockFeeEstimator,
		WalletController:    mockWalletController,
		ConfTarget:          6,
		MinConfs:            1,
		Terms: &batch.Terms{
			OperatorKey: keychain.KeyDescriptor{
				PubKey: operatorPub,
			},
			BoardingExitDelay:          100,
			MinBoardingConfirmations:   1,
			MinVTXOAmount:              1000,
			MaxVTXOAmount:              10000000,
			VTXOExitDelay:              100,
			RegistrationTimeout:        30 * time.Second,
			SignatureCollectionTimeout: 30 * time.Second,
		},
	}

	actorResult := NewActor(cfg)
	actor, err := actorResult.Unpack()
	require.NoError(t, err)

	// Set SelfRef so the actor can receive asynchronous notifications.
	// Done after creation since we need the actor instance.
	cfg.SelfRef = &actorRef{actor: actor}

	h := &actorTestHarness{
		t:                t,
		ctx:              ctx,
		actor:            actor,
		cfg:              cfg,
		locker:           locker,
		clients:          clients,
		chainSource:      chainSource,
		feeEstimator:     mockFeeEstimator,
		walletController: mockWalletController,
		timeoutActor:     timeoutActor,
		operatorPub:      operatorPub,
	}

	// Register cleanup to automatically assert mock expectations.
	t.Cleanup(func() {
		h.assertMockExpectations()
	})

	return h
}

// start initializes the actor by calling Start.
func (h *actorTestHarness) start(ctx context.Context) {
	h.t.Helper()

	err := h.actor.Start(ctx)
	require.NoError(h.t, err)
}

// assertMockExpectations asserts that all mocks received their expected calls.
// This should be called at the end of each test to verify mock expectations.
// Note: mockClientConnRef and mockTimeoutActor are custom mocks that don't use
// testify/mock, so they don't have AssertExpectations methods.
func (h *actorTestHarness) assertMockExpectations() {
	h.t.Helper()

	h.locker.AssertExpectations(h.t)
	h.chainSource.AssertExpectations(h.t)
	h.feeEstimator.AssertExpectations(h.t)
	h.walletController.AssertExpectations(h.t)
}

// assertRoundCount verifies the actor is tracking the expected number of
// rounds.
func (h *actorTestHarness) assertRoundCount(expected int) {
	h.t.Helper()

	require.Len(h.t, h.actor.rounds, expected)
}

// assertCurrentRoundExists verifies the actor has a current round set.
func (h *actorTestHarness) assertCurrentRoundExists() {
	h.t.Helper()

	require.NotNil(h.t, h.actor.currentRound)
}

// sendJoinRequest sends a JoinRoundRequest to the actor.
func (h *actorTestHarness) sendJoinRequest(req *JoinRoundRequest) error {
	h.t.Helper()

	result := h.actor.Receive(h.ctx, req)
	_, err := result.Unpack()

	return err
}

// mockBoardingUTXO sets up the chain source mock to return a UTXO for the
// given outpoint.
//
//nolint:unused
func (h *actorTestHarness) mockBoardingUTXO(outpoint wire.OutPoint,
	clientKey, operatorKey *btcec.PublicKey, exitDelay uint32,
	confirmations int64) {

	h.t.Helper()

	pkScript := buildExpectedPkScript(
		h.t, clientKey, operatorKey, exitDelay,
	)

	h.chainSource.On("GetUTXO", outpoint).Return(
		&UTXO{
			Output: &wire.TxOut{
				Value:    int64(btcutil.Amount(100000)),
				PkScript: pkScript,
			},
			Confirmations: confirmations,
		}, nil,
	)
}

// newClient creates a new clientHarness for testing.
func (h *actorTestHarness) newClient(clientID string,
	baseKeyIndex int32) *actorClientHarness {

	return newActorClientHarness(
		h.t, h, clientID, baseKeyIndex, h.operatorPub,
	)
}

// actorClientHarness is a wrapper around clientHarness that provides
// actor-level test helpers.
type actorClientHarness struct {
	*clientHarness
	harness *actorTestHarness
}

// newActorClientHarness creates a new client harness for actor tests.
func newActorClientHarness(t *testing.T, h *actorTestHarness, clientID string,
	baseKeyIndex int32, operatorKey *btcec.PublicKey) *actorClientHarness {

	const exitDelay = 144
	const expiry = 144

	ch := newClientHarness(
		t, ClientID(clientID), baseKeyIndex, operatorKey, exitDelay,
		expiry,
	)

	return &actorClientHarness{
		clientHarness: ch,
		harness:       h,
	}
}

// mockBoardingUTXO sets up the chain source mock to return a UTXO for the
// given outpoint with the client's boarding key.
func (c *actorClientHarness) mockBoardingUTXO(outpoint wire.OutPoint,
	confirmations int64) {

	c.harness.t.Helper()

	pkScript := buildExpectedPkScript(
		c.harness.t, c.boardingKey, c.operatorKey, c.exitDelay,
	)

	c.harness.chainSource.On("GetUTXO", outpoint).Return(
		&UTXO{
			Output: &wire.TxOut{
				Value:    int64(btcutil.Amount(100000)),
				PkScript: pkScript,
			},
			Confirmations: confirmations,
		}, nil,
	)
}

// createActorJoinRequest creates a JoinRoundRequest for actor-level tests.
func (c *actorClientHarness) createActorJoinRequest(
	boardingReqs []*types.BoardingRequest) *JoinRoundRequest {

	c.t.Helper()

	return &JoinRoundRequest{
		ClientID: c.clientID,
		Request: &types.JoinRoundRequest{
			BoardingReqs: boardingReqs,
		},
	}
}

// TestActorStart verifies that the actor correctly initializes on Start,
// creating a current round FSM.
func TestActorStart(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.start(t.Context())

	// Verify actor created a current round.
	h.assertCurrentRoundExists()
	h.assertRoundCount(1)

	// Verify round ID is set.
	require.NotEmpty(t, h.actor.currentRound.RoundID)
}

// TestActorJoinRoundRequest tests the actor's handling of JoinRoundRequest
// messages.
func TestActorJoinRoundRequest(t *testing.T) {
	t.Parallel()

	t.Run("valid request locks inputs", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.start(h.ctx)

		// Create a client harness.
		client := h.newClient("client1", 10)

		// Set up the outpoint.
		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("test-input")),
			Index: 0,
		}

		// Set up explicit mock expectations.
		roundID := h.actor.currentRound.RoundID
		h.locker.On("IsLocked", mock.Anything, &outpoint).
			Return(false, RoundID{}, nil).Once()
		h.locker.On("Lock", mock.Anything, &outpoint, roundID).
			Return(nil).Once()

		// Mock the UTXO and create the boarding request.
		client.mockBoardingUTXO(outpoint, 10)
		boardingReq := client.createBoardingRequest(&outpoint)
		req := client.createActorJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		)

		// Send the valid request.
		err := h.sendJoinRequest(req)
		require.NoError(t, err)

		// Verify success response was sent to client.
		msgs := h.clients.getMessages()
		require.Len(t, msgs, 1, "expected success response to client")

		successResp, ok := msgs[0].(*clientconn.SendServerEventRequest)
		require.True(t, ok, "expected SendServerEventRequest")

		clientMsg, ok := successResp.Message.(*ClientSuccessResp)
		require.True(t, ok, "expected ClientSuccessResp message")
		require.Equal(t, "client1", string(clientMsg.Client))
		require.Equal(t, roundID, clientMsg.RoundID)

		// Verify timeout was scheduled.
		h.timeoutActor.assertTimeoutScheduled(
			t, roundID, TimeoutPhaseRegistration,
		)
	})

	t.Run("invalid request sends error", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.start(h.ctx)

		// Create a boarding request with a mismatched operator key.
		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("test-input")),
			Index: 0,
		}

		wrongOperatorPub, _ := testutils.CreateKey(999)
		clientKey, _ := testutils.CreateKey(2)

		// Set up IsLocked expectation so validation can proceed to
		// operator key check, which will fail.
		h.locker.On("IsLocked", mock.Anything, &outpoint).
			Return(false, RoundID{}, nil).Once()

		req := &JoinRoundRequest{
			ClientID: "client1",
			Request: &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{
						Outpoint:    &outpoint,
						ClientKey:   clientKey,
						OperatorKey: wrongOperatorPub,
						ExitDelay:   144,
					},
				},
			},
		}

		// Send the invalid request.
		err := h.sendJoinRequest(req)
		require.NoError(t, err) // Receive always succeeds

		// Verify Lock was not called (validation failed before
		// locking).
		h.locker.AssertNotCalled(t, "Lock")

		// Verify error response was sent to client.
		msgs := h.clients.getMessages()
		require.Len(t, msgs, 1, "expected error response to client")

		errorResp, ok := msgs[0].(*clientconn.SendServerEventRequest)
		require.True(t, ok, "expected SendServerEventRequest")

		clientMsg, ok := errorResp.Message.(*ClientErrorResp)
		require.True(t, ok, "expected ClientErrorResp message")
		require.Equal(t, "client1", string(clientMsg.Client))
		require.Contains(t, clientMsg.ErrorMsg, "does not match")
	})
}

// TestActorRegistrationTimeout tests the actor's handling of registration
// timeouts, including the creation of a new round after sealing.
func TestActorRegistrationTimeout(t *testing.T) {
	t.Parallel()

	t.Run("timeout seals round and creates new one", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.start(h.ctx)

		// Create a client and join the round.
		client := h.newClient("client1", 10)
		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("test-input")),
			Index: 0,
		}

		// Set up explicit mock expectations.
		originalRoundID := h.actor.currentRound.RoundID
		h.locker.On("IsLocked", mock.Anything, &outpoint).
			Return(false, RoundID{}, nil).Once()
		h.locker.On("Lock", mock.Anything, &outpoint, originalRoundID).
			Return(nil).Once()
		h.feeEstimator.On("EstimateFeePerKW", uint32(6)).
			Return(chainfee.SatPerKWeight(1000), nil).Once()
		h.walletController.On(
			"FundPsbt", mock.Anything, mock.Anything, int32(1),
			chainfee.SatPerKWeight(1000), mock.Anything,
		).Return(int32(-1), nil).Once()

		client.mockBoardingUTXO(outpoint, 10)
		boardingReq := client.createBoardingRequest(&outpoint)
		req := client.createActorJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		)

		err := h.sendJoinRequest(req)
		require.NoError(t, err)

		// Verify timeout was scheduled.
		h.timeoutActor.assertTimeoutScheduled(
			t, originalRoundID, TimeoutPhaseRegistration,
		)

		// Fire the registration timeout.
		h.timeoutActor.FireTimeout(
			h.ctx, originalRoundID, TimeoutPhaseRegistration,
		)

		// Verify a new round was created.
		require.NotEqual(
			t, originalRoundID, h.actor.currentRound.RoundID,
			"current round should be different after timeout",
		)

		// Verify we now have 2 rounds (original sealed + new).
		h.assertRoundCount(2)

		// Verify the original round is still tracked.
		originalRound := h.actor.getRound(originalRoundID)
		require.NotNil(t, originalRound,
			"original round should still be tracked")
	})

	t.Run("duplicate timeout same phase ignored", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.start(h.ctx)

		// Create a client and join the round.
		client := h.newClient("client1", 10)
		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("test-input")),
			Index: 0,
		}

		// Set up explicit mock expectations.
		originalRoundID := h.actor.currentRound.RoundID
		h.locker.On("IsLocked", mock.Anything, &outpoint).
			Return(false, RoundID{}, nil).Once()
		h.locker.On("Lock", mock.Anything, &outpoint, originalRoundID).
			Return(nil).Once()
		h.feeEstimator.On("EstimateFeePerKW", uint32(6)).
			Return(chainfee.SatPerKWeight(1000), nil).Once()
		h.walletController.On(
			"FundPsbt", mock.Anything, mock.Anything, int32(1),
			chainfee.SatPerKWeight(1000), mock.Anything,
		).Return(int32(-1), nil).Once()

		client.mockBoardingUTXO(outpoint, 10)
		boardingReq := client.createBoardingRequest(&outpoint)
		req := client.createActorJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		)

		err := h.sendJoinRequest(req)
		require.NoError(t, err)

		// Manually create and store a callback (simulating what happens
		// when the timeout is scheduled).
		timeoutID := makeTimeoutID(
			originalRoundID, TimeoutPhaseRegistration,
		)

		// Fire the registration timeout.
		h.timeoutActor.FireTimeout(
			h.ctx, originalRoundID, TimeoutPhaseRegistration,
		)

		// Verify we have 2 rounds after first timeout.
		h.assertRoundCount(2)
		newRoundID := h.actor.currentRound.RoundID

		// Now simulate a duplicate/stale timeout arriving. Since the
		// callback was already removed, manually send the TimeoutMsg.
		duplicateMsg := &TimeoutMsg{
			TimeoutID: timeoutID,
		}
		result := h.actor.Receive(h.ctx, duplicateMsg)
		_, err = result.Unpack()
		require.NoError(t, err, "duplicate timeout should not error")

		// Verify state is unchanged - still 2 rounds, same current.
		h.assertRoundCount(2)
		require.Equal(t, newRoundID, h.actor.currentRound.RoundID,
			"current round should be unchanged after duplicate")
	})

	t.Run("timeout for unknown round ignored", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.start(h.ctx)

		// Send timeout for a round that doesn't exist.
		unknownRoundID, err := NewRoundID()
		require.NoError(t, err)
		timeoutID := makeTimeoutID(
			unknownRoundID, TimeoutPhaseRegistration,
		)

		msg := &TimeoutMsg{
			TimeoutID: timeoutID,
		}

		result := h.actor.Receive(h.ctx, msg)
		_, err = result.Unpack()
		require.NoError(t, err,
			"unknown round timeout should not error")

		// Verify no state changes.
		h.assertRoundCount(1)
	})
}
