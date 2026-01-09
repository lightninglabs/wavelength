package rounds

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightninglabs/darepo/timeout"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
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
func (m *mockClientConnRef) clearMessages() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = nil
}

// getClientBatchInfo extracts the ClientBatchInfo for a specific client from
// the captured messages. Returns nil if not found.
func (m *mockClientConnRef) getClientBatchInfo(
	clientID ClientID) *ClientBatchInfo {

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, msg := range m.messages {
		sendReq, ok := msg.(*clientconn.SendServerEventRequest)
		if !ok {
			continue
		}

		batchInfo, ok := sendReq.Message.(*ClientBatchInfo)
		if !ok {
			continue
		}

		if batchInfo.Client == clientID {
			return batchInfo
		}
	}

	return nil
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

// assertTimeoutCancelled verifies that a timeout was cancelled for the given
// round ID and phase.
func (m *mockTimeoutActor) assertTimeoutCancelled(roundID RoundID,
	phase TimeoutPhase) {

	m.t.Helper()

	id := makeTimeoutID(roundID, phase)

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, cancelledID := range m.cancelledIDs {
		if cancelledID == id {
			return
		}
	}

	m.t.Fatalf("expected timeout cancelled for ID %s", id)
}

// actorTestHarness provides test infrastructure for the rounds Actor.
type actorTestHarness struct {
	*testing.T
	*commonMockSetup

	//nolint:containedctx
	ctx              context.Context
	actor            *Actor
	cfg              *ActorConfig
	clients          *mockClientConnRef
	timeoutActor     *mockTimeoutActor
	chainSourceActor *mockChainSourceActor
}

// newActorTestHarness creates a new actor test harness with default
// configuration.
func newActorTestHarness(t *testing.T) *actorTestHarness {
	t.Helper()

	ctx := t.Context()

	// Create common mock setup.
	common := newCommonMockSetup(t)

	clients := newMockClientConnRef(t)
	timeoutActor := newMockTimeoutActor(t)
	chainSourceActor := newMockChainSourceActor()

	cfg := &ActorConfig{
		ChainParams:         &chaincfg.RegressionNetParams,
		Logger:              btclog.Disabled,
		ClientsConn:         clients,
		BoardingInputLocker: common.boardingLocker,
		ChainSource:         common.chainSource,
		TimeoutActor:        timeoutActor,
		FeeEstimator:        common.feeEstimator,
		WalletController:    common.walletController,
		RoundStore:          common.roundStore,
		VTXOStore:           common.vtxoStore,
		ChainSourceActor:    chainSourceActor,
		ConfTarget:          6,
		MinConfs:            1,
		ConfirmationTarget:  1,
		Terms: &batch.Terms{
			OperatorKey: keychain.KeyDescriptor{
				PubKey: common.operatorPub,
			},
			MaxConnectorsPerTree: 128,
			ConnectorDustAmount:  330,
			ConnectorAddress: mustTaprootAddr(
				t, common.operatorPub,
			),
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
		T:                t,
		commonMockSetup:  common,
		ctx:              ctx,
		actor:            actor,
		cfg:              cfg,
		clients:          clients,
		timeoutActor:     timeoutActor,
		chainSourceActor: chainSourceActor,
	}

	return h
}

// start initializes the actor by calling Start.
func (h *actorTestHarness) start(ctx context.Context) {
	h.Helper()

	err := h.actor.Start(ctx)
	require.NoError(h, err)
}

// assertRoundCount verifies the actor is tracking the expected number of
// rounds.
func (h *actorTestHarness) assertRoundCount(expected int) {
	h.Helper()

	require.Len(h, h.actor.rounds, expected)
}

// assertCurrentRoundExists verifies the actor has a current round set.
func (h *actorTestHarness) assertCurrentRoundExists() {
	h.Helper()

	require.NotNil(h, h.actor.currentRound)
}

// sendJoinRequest sends a JoinRoundRequest to the actor.
func (h *actorTestHarness) sendJoinRequest(req *JoinRoundRequest) error {
	h.Helper()

	result := h.actor.Receive(h.ctx, req)
	_, err := result.Unpack()

	return err
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

// createActorJoinRequest creates a JoinRoundRequest for actor-level tests.
func (c *actorClientHarness) createActorJoinRequest(
	boardingReqs []*types.BoardingRequest) *JoinRoundRequest {

	c.t.Helper()

	// Store the boarding requests for later signature creation.
	c.submittedBoardingReqs = append(
		c.submittedBoardingReqs, boardingReqs...,
	)

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
	h.setActiveRounds([]*Round{})
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
		h.setActiveRounds([]*Round{})
		h.start(h.ctx)

		// Create a client harness.
		client := h.newClient("client1", 10)

		// Set up the outpoint.
		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("test-input")),
			Index: 0,
		}

		// Set up valid boarding input (no batch building since test
		// doesn't seal the round).
		roundID := h.actor.currentRound.RoundID
		h.allowBoardingInput(&outpoint, roundID)

		// Mock the UTXO and create the boarding request.
		h.mockBoardingUTXO(
			outpoint, client.boardingKey, client.exitDelay, 10,
		)
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
		h.setActiveRounds([]*Round{})
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
		h.allowBoardingInput(&outpoint)

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
		h.boardingLocker.AssertNotCalled(t, "Lock")

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
		h.setActiveRounds([]*Round{})
		h.start(h.ctx)

		// Create a client and join the round.
		client := h.newClient("client1", 10)
		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("test-input")),
			Index: 0,
		}

		// Set up complete registration flow.
		originalRoundID := h.actor.currentRound.RoundID
		h.setupCompleteRegistrationFlow(
			&outpoint, client.boardingKey, client.exitDelay, 10,
			originalRoundID,
		)

		// Create the boarding request.
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
		h.setActiveRounds([]*Round{})
		h.start(h.ctx)

		// Create a client and join the round.
		client := h.newClient("client1", 10)
		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("test-input")),
			Index: 0,
		}

		// Set up complete registration flow.
		originalRoundID := h.actor.currentRound.RoundID
		h.setupCompleteRegistrationFlow(
			&outpoint, client.boardingKey, client.exitDelay, 10,
			originalRoundID,
		)

		// Create the boarding request.
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
		h.setActiveRounds([]*Round{})
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

// TestActorFailureHandling tests the actor's handling of round failures,
// including unlocking boarding inputs and creating new rounds.
func TestActorFailureHandling(t *testing.T) {
	t.Parallel()

	t.Run("batch failure unlocks inputs", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setActiveRounds([]*Round{})
		h.start(h.ctx)

		// Create a client and join the round.
		client := h.newClient("client1", 10)
		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("test-input")),
			Index: 0,
		}

		// Set up boarding input with unlock (for failure scenario).
		originalRoundID := h.actor.currentRound.RoundID
		h.setupBoardingInputWithUnlock(
			&outpoint, client.boardingKey, client.exitDelay, 10,
			originalRoundID,
		)

		// Create the boarding request.
		boardingReq := client.createBoardingRequest(&outpoint)
		req := client.createActorJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		)

		err := h.sendJoinRequest(req)
		require.NoError(t, err)

		// Clear client messages from successful join.
		h.clients.clearMessages()

		// Set up expectations for batch building.
		h.setupBatchBuildingFailure(fmt.Errorf("insufficient funds"))

		// Fire the registration timeout to trigger batch building.
		h.timeoutActor.FireTimeout(
			h.ctx, originalRoundID, TimeoutPhaseRegistration,
		)

		// Verify client received failure notification.
		msgs := h.clients.getMessages()
		require.GreaterOrEqual(t, len(msgs), 1,
			"expected at least one message to client")

		// Find the failure notification.
		var failureFound bool
		for _, msg := range msgs {
			sendReq, ok := msg.(*clientconn.SendServerEventRequest)
			if !ok {
				continue
			}

			failResp, ok := sendReq.Message.(*ClientRoundFailedResp)
			if ok {
				require.Equal(t, "client1",
					string(failResp.Client))
				require.Equal(t, originalRoundID,
					failResp.RoundID)
				require.Contains(t, failResp.Reason,
					"insufficient funds")
				failureFound = true

				break
			}
		}
		require.True(t, failureFound, "expected failure resp")

		// Verify failed round was removed and new round created.
		require.Nil(t, h.actor.getRound(originalRoundID),
			"failed round should be removed")
		newRoundID := h.actor.currentRound.RoundID
		require.NotEqual(t, originalRoundID, newRoundID,
			"new round should have different ID")

		// Should still have 1 round (old one removed, new one created).
		h.assertRoundCount(1)
	})
}

// mockChainSourceActor implements actor.ActorRef for testing transaction
// broadcasting and confirmation subscriptions. It records all requests made
// to it so tests can verify the actor's behavior.
type mockChainSourceActor struct {
	mu sync.Mutex

	// broadcastReqs stores all broadcast requests received via Ask.
	broadcastReqs []*chainsource.BroadcastTxRequest

	// confReqs stores all confirmation subscription requests received via
	// Tell.
	confReqs []*chainsource.RegisterConfRequest
}

// newMockChainSourceActor creates a new mock chain source actor.
func newMockChainSourceActor() *mockChainSourceActor {
	return &mockChainSourceActor{
		broadcastReqs: make([]*chainsource.BroadcastTxRequest, 0),
		confReqs:      make([]*chainsource.RegisterConfRequest, 0),
	}
}

// ID returns the ID of this mock actor.
func (m *mockChainSourceActor) ID() string {
	return "mock-chain-source-actor"
}

// Ask handles broadcast requests and returns success.
func (m *mockChainSourceActor) Ask(_ context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	m.mu.Lock()
	defer m.mu.Unlock()

	req, ok := msg.(*chainsource.BroadcastTxRequest)
	if ok {
		m.broadcastReqs = append(m.broadcastReqs, req)

		// Return success response.
		promise := actor.NewPromise[chainsource.ChainSourceResp]()
		promise.Complete(fn.Ok[chainsource.ChainSourceResp](
			&chainsource.BroadcastTxResponse{},
		))

		return promise.Future()
	}

	// Unexpected message type.
	promise := actor.NewPromise[chainsource.ChainSourceResp]()
	promise.Complete(fn.Err[chainsource.ChainSourceResp](
		fmt.Errorf("unexpected message type: %T", msg),
	))

	return promise.Future()
}

// Tell handles confirmation subscription requests.
func (m *mockChainSourceActor) Tell(_ context.Context,
	msg chainsource.ChainSourceMsg) {

	m.mu.Lock()
	defer m.mu.Unlock()

	req, ok := msg.(*chainsource.RegisterConfRequest)
	if ok {
		m.confReqs = append(m.confReqs, req)
	}
}

// getBroadcastReqs returns a copy of all broadcast requests.
func (m *mockChainSourceActor) getBroadcastReqs() []*chainsource.BroadcastTxRequest { //nolint:ll
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]*chainsource.BroadcastTxRequest{}, m.broadcastReqs...)
}

// getConfReqs returns a copy of all confirmation subscription requests.
func (m *mockChainSourceActor) getConfReqs() []*chainsource.RegisterConfRequest { //nolint:ll
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]*chainsource.RegisterConfRequest{}, m.confReqs...)
}

// TestActorBoardingSignatures tests the actor's handling of boarding signature
// submissions via RoundMsg.
func TestActorBoardingSignatures(t *testing.T) {
	t.Parallel()

	t.Run("valid signatures transition to signing", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setActiveRounds([]*Round{})
		h.start(h.ctx)

		// Create a client and join the round with a boarding input.
		client := h.newClient("client1", 10)
		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("boarding-input")),
			Index: 0,
		}

		// Set up complete registration flow.
		roundID := h.actor.currentRound.RoundID
		h.setupCompleteRegistrationFlow(
			&outpoint, client.boardingKey, client.exitDelay, 10,
			roundID,
		)

		// We expect the round building and signing to succeed and
		// therefore for the round to be persisted.
		finalTx := wire.NewMsgTx(2)
		finalTx.AddTxOut(&wire.TxOut{
			Value:    100000,
			PkScript: []byte{0x00, 0x14, 0x01, 0x02, 0x03},
		})
		h.expectRoundFinalized(finalTx)

		// Create the boarding request.
		boardingReq := client.createBoardingRequest(&outpoint)
		req := client.createActorJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		)

		err := h.sendJoinRequest(req)
		require.NoError(t, err)

		// Fire the registration timeout to seal and build the batch.
		h.timeoutActor.FireTimeout(
			h.ctx, roundID, TimeoutPhaseRegistration,
		)

		// Verify the round is now in AwaitingInputSigsState.
		round := h.actor.getRound(roundID)
		require.NotNil(t, round, "round should exist")

		currentState, err := round.FSM.CurrentState()
		require.NoError(t, err)
		_, ok := currentState.(*AwaitingInputSigsState)
		require.True(t, ok, "should be in AwaitingInputSigsState")

		// Verify boarding signatures timeout was scheduled.
		h.timeoutActor.assertTimeoutScheduled(
			t, roundID, TimeoutPhaseInputSigs,
		)

		// Get the ClientBatchInfo that was sent to the client. This
		// mimics the real flow where the client receives the PSBT via
		// the ClientBatchInfo message.
		batchInfo := h.clients.getClientBatchInfo(client.clientID)
		require.NotNil(t, batchInfo, "client should have received "+
			"ClientBatchInfo")
		require.NotNil(t, batchInfo.BatchPSBT, "BatchPSBT should not "+
			"be nil")

		// Create signatures using the PSBT from ClientBatchInfo. This
		// mimics how a real client would create signatures using the
		// batch info they received.
		sigEvent := client.createBoardingSignaturesFromPSBT(
			batchInfo.BatchPSBT,
		)
		roundMsg := &RoundMsg{
			RoundID: roundID,
			Event:   sigEvent,
		}

		result := h.actor.Receive(h.ctx, roundMsg)
		_, err = result.Unpack()
		require.NoError(t, err, "submitting signatures should succeed")

		// Verify the round transitioned through ServerSigningState to
		// FinalizedState.
		newState, err := round.FSM.CurrentState()
		require.NoError(t, err)
		require.IsType(t, &FinalizedState{}, newState)

		// Verify the boarding signatures timeout was cancelled.
		h.timeoutActor.assertTimeoutCancelled(
			roundID, TimeoutPhaseInputSigs,
		)
	})

	t.Run("signatures for unknown round rejected", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setActiveRounds([]*Round{})
		h.start(h.ctx)

		// Send signatures for a round that doesn't exist.
		unknownRoundID, err := NewRoundID()
		require.NoError(t, err)

		sigEvent := &ClientBoardingSignaturesEvent{
			ClientID:   "client1",
			Signatures: nil,
		}
		roundMsg := &RoundMsg{
			RoundID: unknownRoundID,
			Event:   sigEvent,
		}

		result := h.actor.Receive(h.ctx, roundMsg)
		_, err = result.Unpack()
		require.Error(t, err, "unknown round should error")
		require.Contains(t, err.Error(), "not found")
	})
}

// TestActorLoadPendingRounds tests that the actor correctly loads
// pending rounds from storage on startup.
func TestActorLoadPendingRounds(t *testing.T) {
	t.Parallel()

	t.Run("loads pending rounds on start", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)

		// Create a mock persisted round that should be loaded.
		persistedRoundID, err := NewRoundID()
		require.NoError(t, err)

		finalTx := wire.NewMsgTx(2)
		finalTx.AddTxOut(&wire.TxOut{
			Value:    100000,
			PkScript: []byte{0x00, 0x14, 0x01, 0x02, 0x03},
		})

		persistedRound := &Round{
			RoundID:   persistedRoundID,
			FinalTx:   finalTx,
			VTXOTrees: nil,
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": {},
			},
		}

		// Set the pending rounds before starting the actor.
		h.setActiveRounds([]*Round{persistedRound})

		// Start the actor - it should load the pending round.
		h.start(h.ctx)

		// Verify the loaded round is tracked (plus the new current
		// round).
		h.assertRoundCount(2)

		// Verify the loaded round exists and is in FinalizedState.
		loadedRound := h.actor.getRound(persistedRoundID)
		require.NotNil(t, loadedRound,
			"loaded round should be tracked")

		currentState, err := loadedRound.FSM.CurrentState()
		require.NoError(t, err)

		finalizedState, ok := currentState.(*FinalizedState)
		require.True(t, ok,
			"loaded round should be in FinalizedState, got %T",
			currentState)

		// Verify the state has the correct data.
		require.Equal(t, persistedRound.FinalTx, finalizedState.FinalTx)
		require.Len(t, finalizedState.ClientRegistrations, 1)
	})

	t.Run("starts with no pending rounds", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)

		// Set no pending rounds - LoadPendingRounds returns empty.
		h.setActiveRounds([]*Round{})
		h.start(h.ctx)

		// Should only have the new current round.
		h.assertRoundCount(1)
		h.assertCurrentRoundExists()
	})
}

// TestActorBroadcastAndConfirmation tests the actor's handling of transaction
// broadcast and confirmation flow.
func TestActorBroadcastAndConfirmation(t *testing.T) {
	t.Parallel()

	t.Run("finalized round broadcasts and subscribes", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)

		// Create a round in FinalizedState with a signed transaction.
		persistedRoundID, err := NewRoundID()
		require.NoError(t, err)

		finalTx := wire.NewMsgTx(2)
		finalTx.AddTxOut(&wire.TxOut{
			Value:    100000,
			PkScript: []byte{0x00, 0x14, 0x01, 0x02, 0x03},
		})

		persistedRound := &Round{
			RoundID:   persistedRoundID,
			FinalTx:   finalTx,
			VTXOTrees: nil,
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": {},
			},
		}

		// Set the round in storage and start the actor. This will
		// trigger loadRoundFSM which calls broadcastAndSubscribe.
		h.setActiveRounds([]*Round{persistedRound})
		h.start(h.ctx)

		// Verify the round is loaded and in FinalizedState.
		loadedRound := h.actor.getRound(persistedRoundID)
		require.NotNil(t, loadedRound)

		currentState, err := loadedRound.FSM.CurrentState()
		require.NoError(t, err)
		_, ok := currentState.(*FinalizedState)
		require.True(t, ok)

		// Verify broadcast was requested.
		broadcastReqs := h.chainSourceActor.getBroadcastReqs()
		require.Len(t, broadcastReqs, 1)
		require.Equal(t, finalTx, broadcastReqs[0].Tx)

		// Verify confirmation subscription was registered.
		confReqs := h.chainSourceActor.getConfReqs()
		require.Len(t, confReqs, 1)
		require.Equal(
			t, persistedRoundID.String(), confReqs[0].CallerID,
		)
		require.Equal(t, uint32(1), confReqs[0].TargetConfs)
	})

	t.Run("confirmation transitions to ConfirmedState", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)

		// Create a round in FinalizedState.
		persistedRoundID, err := NewRoundID()
		require.NoError(t, err)

		finalTx := wire.NewMsgTx(2)
		finalTx.AddTxOut(&wire.TxOut{
			Value:    100000,
			PkScript: []byte{0x00, 0x14, 0x01, 0x02, 0x03},
		})

		persistedRound := &Round{
			RoundID:   persistedRoundID,
			FinalTx:   finalTx,
			VTXOTrees: nil,
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": {},
			},
		}

		// Load the round and start the actor.
		h.setActiveRounds([]*Round{persistedRound})
		h.start(h.ctx)

		// Verify round is in FinalizedState.
		loadedRound := h.actor.getRound(persistedRoundID)
		require.NotNil(t, loadedRound)

		currentState, err := loadedRound.FSM.CurrentState()
		require.NoError(t, err)
		_, ok := currentState.(*FinalizedState)
		require.True(t, ok)

		// Send a confirmation message directly to the actor.
		blockHash := chainhash.HashH([]byte("test-block"))
		confMsg := &ConfirmationMsg{
			RoundID:     persistedRoundID,
			BlockHeight: 100,
			BlockHash:   blockHash,
			NumConfs:    1,
		}

		result := h.actor.Receive(h.ctx, confMsg)
		_, err = result.Unpack()
		require.NoError(t, err)

		// Verify round transitioned to ConfirmedState.
		confirmedState, err := loadedRound.FSM.CurrentState()
		require.NoError(t, err)

		confirmed, ok := confirmedState.(*ConfirmedState)
		require.True(t, ok)
		require.Equal(t, int32(100), confirmed.BlockHeight)
		require.Equal(t, blockHash, confirmed.BlockHash)
	})

	t.Run("confirmation for unknown round ignored", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setActiveRounds([]*Round{})
		h.start(h.ctx)

		// Send confirmation for a round that doesn't exist.
		unknownRoundID, err := NewRoundID()
		require.NoError(t, err)

		blockHash := chainhash.HashH([]byte("test-block"))
		confMsg := &ConfirmationMsg{
			RoundID:     unknownRoundID,
			BlockHeight: 100,
			BlockHash:   blockHash,
			NumConfs:    1,
		}

		result := h.actor.Receive(h.ctx, confMsg)
		_, err = result.Unpack()
		require.NoError(t, err)

		// Verify state is unchanged - still just the current round.
		h.assertRoundCount(1)
	})
}
