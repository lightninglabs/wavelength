package round

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/timeout"
	"github.com/lightninglabs/wavelength/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// confEventNotifier is a type alias for confirmation event notifiers.
type confEventNotifier = actor.TellOnlyRef[chainsource.ConfirmationEvent]

// testRoundID creates a deterministic RoundID from a string seed for testing.
// This creates a valid UUID v4 by using the hash of the seed.
func testRoundID(seed string) RoundID {
	// Create a deterministic UUID from the seed by using it as namespace
	// data.
	h := chainhash.HashH([]byte(seed))
	id, _ := uuid.FromBytes(h[:16])

	return RoundID(id)
}

// mockServerConnRef captures all messages sent to the server for test
// verification, implementing actor.TellOnlyRef[serverconn.ServerConnMsg].
type mockServerConnRef struct {
	t        *testing.T
	id       string
	messages []serverconn.ServerConnMsg
	mu       sync.Mutex
}

func newMockServerConnRef(t *testing.T) *mockServerConnRef {
	return &mockServerConnRef{
		t:        t,
		id:       "mock-server-conn",
		messages: make([]serverconn.ServerConnMsg, 0),
	}
}

func (m *mockServerConnRef) ID() string {
	return m.id
}

// Tell records outgoing messages for assertion.
func (m *mockServerConnRef) Tell(
	_ context.Context, msg serverconn.ServerConnMsg,
) error {

	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, msg)

	return nil
}

func (m *mockServerConnRef) clearMessages() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = m.messages[:0]
}

func (m *mockServerConnRef) snapshotMessages() []serverconn.ServerConnMsg {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]serverconn.ServerConnMsg(nil), m.messages...)
}

// assertMessageSent checks that a message of the given type was sent.
func (m *mockServerConnRef) assertMessageSent(t *testing.T, msgType string) {
	t.Helper()

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, msg := range m.messages {
		if msg.MessageType() == msgType {
			return
		}
	}

	t.Fatalf("expected message of type %q, but none found in %d messages",
		msgType, len(m.messages))
}

// mockChainSourceRef captures chain registration requests for test
// verification, implementing actor.TellOnlyRef[chainsource.ChainSourceMsg].
type mockChainSourceRef struct {
	t             *testing.T
	id            string
	registrations []*chainsource.RegisterConfRequest
	mu            sync.Mutex

	// notifiers stores mapped actor refs, enabling the test to inject
	// confirmations back to the actor being tested.
	notifiers map[string]confEventNotifier
}

func newMockChainSourceRef(t *testing.T) *mockChainSourceRef {
	return &mockChainSourceRef{
		t:             t,
		id:            "mock-chain-source",
		registrations: make([]*chainsource.RegisterConfRequest, 0),
		notifiers:     make(map[string]confEventNotifier),
	}
}

func (m *mockChainSourceRef) ID() string {
	return m.id
}

// Tell captures chain source messages for assertion.
func (m *mockChainSourceRef) Tell(
	_ context.Context, msg chainsource.ChainSourceMsg,
) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	if req, ok := msg.(*chainsource.RegisterConfRequest); ok {
		m.registrations = append(m.registrations, req)

		// Store the notifier so tests can inject confirmations back
		// to the actor.
		if req.NotifyActor.IsSome() {
			notifier := req.NotifyActor.UnwrapOrFail(m.t)
			m.notifiers[req.CallerID] = notifier
		}
	}

	return nil
}

// Ask implements actor.ActorRef for the mock. It returns a BestHeightResponse
// for height queries.
func (m *mockChainSourceRef) Ask(
	_ context.Context, msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	promise := actor.NewPromise[chainsource.ChainSourceResp]()

	switch msg.(type) {
	case *chainsource.BestHeightRequest:
		// Return a reasonable test height.
		resp := &chainsource.BestHeightResponse{
			Height: 100,
		}
		promise.Complete(fn.Ok[chainsource.ChainSourceResp](resp))

	default:
		promise.Complete(
			fn.Err[chainsource.ChainSourceResp](
				fmt.Errorf("unexpected Ask message type: %T",
					msg),
			),
		)
	}

	return promise.Future()
}

// mockWalletActorRef captures registration requests and enables tests to
// inject boarding confirmations back to the actor under test.
type mockWalletActorRef struct {
	t  *testing.T
	id string
	mu sync.Mutex

	// registeredNotifier holds the actor ref provided during registration,
	// enabling tests to send boarding confirmations.
	registeredNotifier actor.TellOnlyRef[wallet.BoardingUtxoConfirmedEvent]

	// confirmedIntents are returned to TriggerBoard recovery queries.
	confirmedIntents []wallet.BoardingIntent
}

func newMockWalletActorRef(t *testing.T) *mockWalletActorRef {
	return &mockWalletActorRef{
		t:                t,
		id:               "mock-wallet-actor",
		confirmedIntents: make([]wallet.BoardingIntent, 0),
	}
}

func (m *mockWalletActorRef) ID() string {
	return m.id
}

// Tell implements actor.ActorRef, but is unused in these tests.
func (m *mockWalletActorRef) Tell(_ context.Context,
	msg wallet.WalletMsg) error {

	// WalletActor uses Ask pattern for registration, so Tell is unused in
	// these tests.
	_ = msg

	return nil
}

func (m *mockWalletActorRef) Ask(_ context.Context,
	msg wallet.WalletMsg) actor.Future[wallet.WalletResp] {

	m.mu.Lock()
	defer m.mu.Unlock()

	if req, ok := msg.(*wallet.RegisterConfirmationNotifierRequest); ok {
		m.registeredNotifier = req.NotifyActor

		resp := &wallet.RegisterConfirmationNotifierResponse{
			Success: true,
		}

		return newImmediateFuture[wallet.WalletResp](resp)
	}

	if _, ok := msg.(*wallet.GetConfirmedBoardingIntentsRequest); ok {
		resp := &wallet.GetConfirmedBoardingIntentsResponse{
			Intents: append(
				[]wallet.BoardingIntent(nil),
				m.confirmedIntents...,
			),
		}

		return newImmediateFuture[wallet.WalletResp](resp)
	}

	return newImmediateFuture[wallet.WalletResp](nil)
}

func (m *mockWalletActorRef) setConfirmedIntents(
	intents ...wallet.BoardingIntent) {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.confirmedIntents = append(
		[]wallet.BoardingIntent(nil), intents...,
	)
}

// sendBoardingConfirmation simulates a boarding UTXO confirmation from wallet.
func (m *mockWalletActorRef) sendBoardingConfirmation(ctx context.Context,
	intent *wallet.BoardingIntent) {

	m.mu.Lock()
	notifier := m.registeredNotifier
	m.mu.Unlock()

	if notifier == nil {
		m.t.Fatal("no notifier registered for boarding confirmations")
	}

	event := wallet.BoardingUtxoConfirmedEvent{
		BoardingIntent: intent,
	}
	require.NoError(m.t, notifier.Tell(ctx, event))
}

// mockSelfRef captures messages that the round actor sends to itself,
// enabling tests to intercept and verify self-notifications.
type mockSelfRef struct {
	t        *testing.T
	id       string
	messages []actormsg.RoundReceivable
	msgChan  chan actormsg.RoundReceivable
	mu       sync.Mutex
}

func newMockSelfRef(t *testing.T) *mockSelfRef {
	return &mockSelfRef{
		t:        t,
		id:       "mock-self-ref",
		messages: make([]actormsg.RoundReceivable, 0),
		msgChan:  make(chan actormsg.RoundReceivable, 100),
	}
}

func (m *mockSelfRef) ID() string {
	return m.id
}

// Tell records the message and also forwards it to a buffered channel.
func (m *mockSelfRef) Tell(
	_ context.Context, msg actormsg.RoundReceivable,
) error {

	m.mu.Lock()
	m.messages = append(m.messages, msg)
	m.mu.Unlock()

	// Also send to channel for blocking receives in test assertions.
	select {
	case m.msgChan <- msg:
	default:
	}

	return nil
}

// waitForMessage blocks until a message arrives or the timeout expires,
// enabling synchronous test assertions on asynchronous actor messages.
func (m *mockSelfRef) waitForMessage(timeout time.Duration) (
	actormsg.RoundReceivable, bool) {

	select {
	case msg := <-m.msgChan:
		return msg, true

	case <-time.After(timeout):
		return nil, false
	}
}

// mockTimeoutActor captures timeout schedule/cancel requests and can fire
// expiry callbacks on demand.
type mockTimeoutActor struct {
	t *testing.T

	mu sync.Mutex

	scheduledIDs map[timeout.ID]time.Duration
	cancelledIDs []timeout.ID
	callbacks    map[timeout.ID]actor.TellOnlyRef[*timeout.ExpiredMsg]
}

// newMockTimeoutActor creates a timeout actor test double.
func newMockTimeoutActor(t *testing.T) *mockTimeoutActor {
	return &mockTimeoutActor{
		t:            t,
		scheduledIDs: make(map[timeout.ID]time.Duration),
		cancelledIDs: make([]timeout.ID, 0),
		callbacks: make(
			map[timeout.ID]actor.TellOnlyRef[*timeout.ExpiredMsg],
		),
	}
}

// ID returns the mock actor identifier.
func (m *mockTimeoutActor) ID() string {
	return "mock-timeout-actor"
}

// Tell records timeout actor messages.
func (m *mockTimeoutActor) Tell(_ context.Context, msg timeout.Msg) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch req := msg.(type) {
	case *timeout.ScheduleTimeoutRequest:
		m.scheduledIDs[req.ID] = req.Duration
		m.callbacks[req.ID] = req.Callback

	case *timeout.CancelTimeoutRequest:
		m.cancelledIDs = append(m.cancelledIDs, req.ID)
		delete(m.scheduledIDs, req.ID)
		delete(m.callbacks, req.ID)
	}

	return nil
}

// assertTimeoutScheduled verifies a timeout was scheduled.
func (m *mockTimeoutActor) assertTimeoutScheduled(t *testing.T, id timeout.ID,
	expected time.Duration) {

	t.Helper()

	m.mu.Lock()
	defer m.mu.Unlock()

	duration, ok := m.scheduledIDs[id]
	require.True(t, ok, "expected timeout scheduled for ID %s", id)
	require.Equal(t, expected, duration)
}

// assertTimeoutCancelled verifies a timeout was cancelled.
func (m *mockTimeoutActor) assertTimeoutCancelled(t *testing.T, id timeout.ID) {
	t.Helper()

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, cancelled := range m.cancelledIDs {
		if cancelled == id {
			return
		}
	}

	t.Fatalf("expected timeout cancelled for ID %s", id)
}

// mockVTXOManagerRef captures messages sent to the VTXO manager for test
// verification, implementing actor.TellOnlyRef[VTXOManagerMsg].
type mockVTXOManagerRef struct {
	t        *testing.T
	id       string
	messages []actor.Message
	mu       sync.Mutex
}

func newMockVTXOManagerRef(t *testing.T) *mockVTXOManagerRef {
	return &mockVTXOManagerRef{
		t:        t,
		id:       "mock-vtxo-manager",
		messages: make([]actor.Message, 0),
	}
}

func (m *mockVTXOManagerRef) ID() string {
	return m.id
}

func (m *mockVTXOManagerRef) Tell(_ context.Context, msg VTXOManagerMsg) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, msg)

	return nil
}

// assertVTXOCreatedReceived verifies a VTXOCreatedNotification was received.
func (m *mockVTXOManagerRef) assertVTXOCreatedReceived(
	t *testing.T,
) *VTXOCreatedNotification {

	t.Helper()

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, msg := range m.messages {
		if notif, ok := msg.(*VTXOCreatedNotification); ok {
			return notif
		}
	}

	t.Fatalf("expected VTXOCreatedNotification, none found in %d messages",
		len(m.messages))

	return nil
}

// immediateFuture provides a synchronous Future implementation for tests,
// avoiding the complexity of async futures when the result is already known.
type immediateFuture[T any] struct {
	result fn.Result[T]
}

func newImmediateFuture[T any](val T) actor.Future[T] {
	return &immediateFuture[T]{result: fn.Ok(val)}
}

func (f *immediateFuture[T]) Await(_ context.Context) fn.Result[T] {
	return f.result
}

func (f *immediateFuture[T]) ThenApply(ctx context.Context,
	fn func(T) T) actor.Future[T] {

	val, err := f.result.Unpack()
	if err != nil {
		return f
	}

	return newImmediateFuture[T](fn(val))
}

func (f *immediateFuture[T]) OnComplete(_ context.Context,
	callback func(fn.Result[T])) {

	callback(f.result)
}

// actorTestHarness provides end-to-end testing of RoundClientActor by
// wrapping it with mock dependencies that capture outgoing messages and enable
// injection of incoming events, allowing tests to verify full actor workflows
// without real network or blockchain interactions.
//
//nolint:containedctx
type actorTestHarness struct {
	t   *testing.T
	ctx context.Context

	actor *RoundClientActor

	serverConn   *mockServerConnRef
	chainSource  *mockChainSourceRef
	walletActor  *mockWalletActorRef
	selfRef      *mockSelfRef
	timeoutActor *mockTimeoutActor
	vtxoManager  *mockVTXOManagerRef

	roundStore *MockRoundStore
	vtxoStore  *MockVTXOStore
	wallet     *MockClientWallet

	clientPrivKey   *btcec.PrivateKey
	operatorPrivKey *btcec.PrivateKey
	clientPubKey    *btcec.PublicKey
	operatorPubKey  *btcec.PublicKey

	operatorTerms *types.OperatorTerms
}

func newActorTestHarness(t *testing.T) *actorTestHarness {
	t.Helper()

	clientPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientPubKey := clientPrivKey.PubKey()
	operatorPubKey := operatorPrivKey.PubKey()

	serverConn := newMockServerConnRef(t)
	chainSource := newMockChainSourceRef(t)
	walletActor := newMockWalletActorRef(t)
	selfRef := newMockSelfRef(t)
	timeoutActor := newMockTimeoutActor(t)
	vtxoManager := newMockVTXOManagerRef(t)

	roundStore := &MockRoundStore{}
	vtxoStore := &MockVTXOStore{}
	walletMock := &MockClientWallet{}

	// The join-round transition always derives a fresh identifier key,
	// so wire up a default mock that returns a valid key descriptor.
	identifierPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	walletMock.On(
		"DeriveNextKey", mock.Anything,
		joinRoundAuthIdentifierKeyFamily,
	).Return(&keychain.KeyDescriptor{
		PubKey: identifierPrivKey.PubKey(),
	}, nil)

	operatorTerms := &types.OperatorTerms{
		PubKey:            operatorPubKey,
		BoardingExitDelay: 144,
		VTXOExitDelay:     144,
		MinConfirmations:  1,
	}

	// Default max operator fee for tests: 100,000 sats (0.001 BTC).
	// This is generous to avoid test brittleness when multiple intents
	// are used.
	const defaultMaxOperatorFee = btcutil.Amount(100000)

	cfg := &RoundClientConfig{
		Name:                   "test-round-actor",
		Wallet:                 walletMock,
		RoundStore:             roundStore,
		VTXOStore:              vtxoStore,
		OperatorTerms:          operatorTerms,
		ServerConn:             serverConn,
		ChainSource:            chainSource,
		WalletActor:            walletActor,
		SelfRef:                selfRef,
		TimeoutActor:           timeoutActor,
		VTXOManager:            vtxoManager,
		ChainParams:            &chaincfg.MainNetParams,
		MaxOperatorFee:         defaultMaxOperatorFee,
		DisableJoinRequestAuth: true,
	}

	actorResult := NewRoundClientActor(cfg)
	require.True(
		t, actorResult.IsOk(),
		"failed to create actor: %v", actorResult.Err(),
	)

	actor := actorResult.UnwrapOrFail(t)

	return &actorTestHarness{
		t:               t,
		ctx:             t.Context(),
		actor:           actor,
		serverConn:      serverConn,
		chainSource:     chainSource,
		walletActor:     walletActor,
		selfRef:         selfRef,
		timeoutActor:    timeoutActor,
		vtxoManager:     vtxoManager,
		roundStore:      roundStore,
		vtxoStore:       vtxoStore,
		wallet:          walletMock,
		clientPrivKey:   clientPrivKey,
		operatorPrivKey: operatorPrivKey,
		clientPubKey:    clientPubKey,
		operatorPubKey:  operatorPubKey,
		operatorTerms:   operatorTerms,
	}
}

// newActorTestHarnessWithRealTimeout creates an actor test harness that uses
// a real timeout.Actor registered in a real ActorSystem instead of a mock.
// This exercises the full timer callback flow through the actor system.
func newActorTestHarnessWithRealTimeout(t *testing.T) *actorTestHarness {
	t.Helper()

	h := newActorTestHarness(t)

	// Replace the mock timeout actor with a real one wired through
	// an actor system.
	system := actor.NewActorSystem()
	t.Cleanup(func() {
		_ = system.Shutdown(context.Background())
	})

	realTimeoutActor := timeout.NewActor()
	timeoutKey := actor.NewServiceKey[timeout.Msg, timeout.Resp](
		"test-timeout",
	)
	realTimeoutRef := actor.RegisterWithSystem(
		system, "test-timeout", timeoutKey, realTimeoutActor,
	)
	realTimeoutActor.Start(realTimeoutRef)

	h.actor.cfg.TimeoutActor = realTimeoutRef

	return h
}

// setupMockRoundStoreForStart configures the RoundStore mock to return no
// active rounds, simulating a fresh start with no recovery needed.
func (h *actorTestHarness) setupMockRoundStoreForStart() {
	h.t.Helper()

	h.roundStore.On(
		"ListActiveRounds", mock.Anything,
	).Return([]*Round{}, nil)
}

// start initializes the actor by calling Start().
func (h *actorTestHarness) start() error {
	return h.actor.Start(h.ctx)
}

// receive sends a message to the actor and returns the response.
func (h *actorTestHarness) receive(
	msg actormsg.RoundReceivable,
) fn.Result[actormsg.RoundActorResp] {

	return h.actor.Receive(h.ctx, msg)
}

// sendWalletConfirmation simulates a boarding UTXO confirmation event from the
// wallet actor, then forwards the self-notification back to the actor to
// complete the round-trip.
func (h *actorTestHarness) sendWalletConfirmation(
	intent *wallet.BoardingIntent) {

	h.walletActor.sendBoardingConfirmation(h.ctx, intent)

	msg, ok := h.selfRef.waitForMessage(time.Second)
	require.True(h.t, ok, "expected WalletBoardingConfirmed message")

	result := h.receive(msg)
	require.True(
		h.t, result.IsOk(),
		"actor receive failed: %v", result.Err(),
	)
}

// sendServerMessage wraps a server event in a notification and delivers it to
// the actor, simulating server-to-client communication.
func (h *actorTestHarness) sendServerMessage(event ClientEvent) {
	msg := &ServerMessageNotification{Message: event}
	result := h.receive(msg)
	require.True(
		h.t, result.IsOk(),
		"actor receive failed: %v", result.Err(),
	)
}

// queryState retrieves the current FSM states for test assertions.
func (h *actorTestHarness) queryState() map[string]FSMStateInfo {
	result := h.receive(&GetClientStateRequest{})
	require.True(h.t, result.IsOk())

	resp, _ := result.Unpack()
	stateResp, ok := resp.(*GetClientStateResponse)
	require.True(h.t, ok, "expected GetClientStateResponse")

	return stateResp.States
}

// findTempState returns the first temp-keyed round's state from the states map.
// Returns the state info and true if found, empty and false otherwise.
func (h *actorTestHarness) findTempState(states map[string]FSMStateInfo) (
	FSMStateInfo, bool) {

	h.t.Helper()

	for _, state := range states {
		if state.IsTemp {
			return state, true
		}
	}

	return FSMStateInfo{}, false
}

// newTestBoardingIntent creates a complete boarding intent with proper
// tapscript and chain info for testing.
func (h *actorTestHarness) newTestBoardingIntent() *wallet.BoardingIntent {
	return h.newTestBoardingIntentWithSuffix("")
}

// newTestBoardingIntentWithSuffix creates a unique boarding intent, using the
// suffix to generate distinct outpoints when multiple intents are needed.
func (h *actorTestHarness) newTestBoardingIntentWithSuffix(
	suffix string) *wallet.BoardingIntent {

	h.t.Helper()

	hash := chainhash.HashH([]byte(h.t.Name() + "-intent" + suffix))
	outpoint := wire.OutPoint{Hash: hash, Index: 0}

	tapscript, err := arkscript.VTXOTapScript(
		h.clientPubKey, h.operatorPubKey, h.operatorTerms.VTXOExitDelay,
	)
	require.NoError(h.t, err)

	taprootKey, err := tapscript.TaprootKey()
	require.NoError(h.t, err)

	addr, err := btcaddr.NewAddressTaproot(
		taprootKey.SerializeCompressed()[1:], &chaincfg.MainNetParams,
	)
	require.NoError(h.t, err)

	keyDesc := keychain.KeyDescriptor{
		PubKey: h.clientPubKey,
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(wallet.BoardingKeyFamily),
			Index:  0,
		},
	}

	boardingAddr := wallet.BoardingAddress{
		Address:     addr,
		Tapscript:   tapscript,
		KeyDesc:     keyDesc,
		OperatorKey: h.operatorPubKey,
		ExitDelay:   h.operatorTerms.VTXOExitDelay,
	}

	pkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(h.t, err)

	confTx := wire.NewMsgTx(2)
	confTx.AddTxOut(&wire.TxOut{
		Value:    50000,
		PkScript: pkScript,
	})

	return &wallet.BoardingIntent{
		Address:  boardingAddr,
		Outpoint: outpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: 100,
			OutPoint:   outpoint,
			Amount:     btcutil.Amount(50000),
			ConfTx:     confTx,
		},
		Status: wallet.BoardingStatusConfirmed,
	}
}

// newKeyDescriptor creates a keychain.KeyDescriptor using the harness's client
// key, useful for creating test ClientVTXO structs.
func (h *actorTestHarness) newKeyDescriptor() keychain.KeyDescriptor {
	return keychain.KeyDescriptor{
		PubKey: h.clientPubKey,
		KeyLocator: keychain.KeyLocator{
			Family: types.VTXOSigningKeyFamily,
			Index:  0,
		},
	}
}

// simulateRoundJoined injects a RoundJoined server event to the actor,
// simulating successful round enrollment. The outpoints are used to correlate
// the response to the correct pending round.
func (h *actorTestHarness) simulateRoundJoined(roundID RoundID,
	boardingOutpoints []wire.OutPoint) {

	h.t.Helper()

	event := &RoundJoined{
		RoundID:                   roundID,
		AcceptedBoardingOutpoints: boardingOutpoints,
		AcceptedVTXOOutpoints:     nil,
	}
	h.sendServerMessage(event)
}

// assertFSMState verifies that at least one FSM is in the expected state type.
func (h *actorTestHarness) assertFSMState(expectedStateType string) {
	h.t.Helper()

	states := h.queryState()
	require.NotEmpty(h.t, states, "expected at least one FSM")

	for _, stateInfo := range states {
		stateName := fmt.Sprintf("%T", stateInfo.State)
		if strings.Contains(stateName, expectedStateType) {
			return
		}
	}

	// No matching state found, print what we have for debugging.
	var foundStates []string
	for key, stateInfo := range states {
		stateStr := fmt.Sprintf("%s=%T", key, stateInfo.State)

		// Print additional info for ClientFailedState.
		if failedState, ok := stateInfo.State.(*ClientFailedState); ok {
			stateStr = fmt.Sprintf("%s (reason=%q, err=%v)",
				stateStr, failedState.Reason, failedState.Error)
		}

		foundStates = append(foundStates, stateStr)
	}
	h.t.Fatalf("expected state containing %q, got: %v", expectedStateType,
		foundStates)
}

// assertServerMessageSent verifies a message of the given type was sent to
// the server.
func (h *actorTestHarness) assertServerMessageSent(msgType string) {
	h.t.Helper()
	h.serverConn.assertMessageSent(h.t, msgType)
}

// clearServerMessages resets the captured server messages, allowing tests to
// verify only new messages sent after this point.
func (h *actorTestHarness) clearServerMessages() {
	h.serverConn.clearMessages()
}

// serverMessages returns a snapshot of messages sent to the server.
func (h *actorTestHarness) serverMessages() []serverconn.ServerConnMsg {
	h.t.Helper()

	return h.serverConn.snapshotMessages()
}

// sendVTXORequests sends VTXO request amounts to the actor. This sets up the
// mock wallet to return key descriptors for each amount, then sends a
// RegisterVTXORequestsRequest message.
func (h *actorTestHarness) sendVTXORequests(amounts ...btcutil.Amount) {
	h.t.Helper()

	// Setup mock to return a key descriptor for each DeriveNextKey call.
	for i := range amounts {
		keyDesc := &keychain.KeyDescriptor{
			PubKey: h.clientPubKey,
			KeyLocator: keychain.KeyLocator{
				Family: types.VTXOOwnerKeyFamily,
				Index:  uint32(i),
			},
		}
		h.wallet.On(
			"DeriveNextKey", mock.Anything,
			types.VTXOOwnerKeyFamily,
		).Return(keyDesc, nil).Once()
	}

	msg := &RegisterVTXORequestsRequest{Amounts: amounts}
	result := h.receive(msg)
	require.True(
		h.t, result.IsOk(),
		"actor receive failed: %v", result.Err(),
	)
}

// newTestRound creates a test Round with a unique commitment transaction,
// using the roundID in the script to ensure distinct transaction hashes.
func (h *actorTestHarness) newTestRound(roundID RoundID) *Round {
	h.t.Helper()

	tx := wire.NewMsgTx(2)

	uniqueScript := append([]byte{0x00, 0x14}, []byte(roundID.String())...)
	tx.AddTxOut(&wire.TxOut{
		Value:    100000,
		PkScript: uniqueScript,
	})

	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(h.t, err)

	return &Round{
		RoundID:      roundID,
		CommitmentTx: fn.Some(packet),
	}
}

// setupRoundInInputSigSentState creates a round FSM in InputSigSentState and
// adds it to the actor's rounds map. This allows testing the checkpoint flow
// where the FSM is already at the checkpoint state. Returns the PSBT
// commitment tx for test assertions.
func (h *actorTestHarness) setupRoundInInputSigSentState(
	roundID RoundID,
) *psbt.Packet {

	h.t.Helper()

	// Create a unique commitment tx for this round.
	tx := wire.NewMsgTx(2)
	uniqueScript := append([]byte{0x00, 0x14}, []byte(roundID.String())...)
	tx.AddTxOut(&wire.TxOut{
		Value:    100000,
		PkScript: uniqueScript,
	})
	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(h.t, err)

	// Create InputSigSentState with the commitment tx.
	initialState := &InputSigSentState{
		RoundID:      roundID,
		CommitmentTx: packet,
	}

	// Create a new FSM starting in InputSigSentState.
	errReporter := newContextErrorReporter(
		h.ctx, roundID.LogPrefix(),
	)
	fsmCfg := ClientStateMachineCfg{
		Logger:        h.actor.log.WithPrefix(roundID.LogPrefix()),
		ErrorReporter: errReporter,
		InitialState:  initialState,
		Env:           h.actor.env,
	}
	newFSM := protofsm.NewStateMachine(fsmCfg)
	newFSM.Start(h.ctx)

	// Add to the actor's rounds map.
	keyStr := RoundKeyStr(roundID.KeyString())
	h.actor.rounds[keyStr] = &RoundFSM{
		FSM:     &newFSM,
		Key:     roundID,
		RoundID: roundID,
	}

	return packet
}

// setupRoundInForfeitCollectingState creates a round FSM in
// ForfeitSignaturesCollectingState and adds it to the actor's rounds map.
func (h *actorTestHarness) setupRoundInForfeitCollectingState(roundID RoundID) {
	h.t.Helper()

	tx := wire.NewMsgTx(2)
	uniqueScript := append([]byte{0x00, 0x14}, []byte(roundID.String())...)
	tx.AddTxOut(&wire.TxOut{
		Value:    100000,
		PkScript: uniqueScript,
	})
	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(h.t, err)

	vtxoHash := chainhash.HashH(
		[]byte("forfeit-vtxo-" + roundID.String()),
	)
	vtxoOutpoint := wire.OutPoint{
		Hash:  vtxoHash,
		Index: 0,
	}

	connHash := chainhash.HashH(
		[]byte("connector-" + roundID.String()),
	)
	connectorOutpoint := wire.OutPoint{
		Hash:  connHash,
		Index: 1,
	}
	initialState := &ForfeitSignaturesCollectingState{
		RoundID:      roundID,
		CommitmentTx: packet,
		ExpectedForfeits: map[wire.OutPoint]*ConnectorLeafInfo{
			vtxoOutpoint: {
				LeafIndex:         0,
				ConnectorOutpoint: connectorOutpoint,
				ConnectorPkScript: []byte{
					0x51,
					0x20,
				},
				ConnectorAmount: 546,
				VTXOAmount:      50000,
			},
		},
		CollectedForfeits: make(
			map[wire.OutPoint]*ForfeitSignatureResponse,
		),
	}

	errReporter := newContextErrorReporter(
		h.ctx, roundID.LogPrefix(),
	)
	fsmCfg := ClientStateMachineCfg{
		Logger:        h.actor.log.WithPrefix(roundID.LogPrefix()),
		ErrorReporter: errReporter,
		InitialState:  initialState,
		Env:           h.actor.env,
	}
	newFSM := protofsm.NewStateMachine(fsmCfg)
	newFSM.Start(h.ctx)

	keyStr := RoundKeyStr(roundID.KeyString())
	h.actor.rounds[keyStr] = &RoundFSM{
		FSM:     &newFSM,
		Key:     roundID,
		RoundID: roundID,
	}
}

// setupRoundInIntentSentState creates a round FSM in
// IntentSentState with a temp key and adds it to the actor's rounds map.
// This simulates a round that has been sent to the server but not yet assigned
// a RoundID.
func (h *actorTestHarness) setupRoundInIntentSentState() TempRoundKey {
	h.t.Helper()

	tempKey, err := NewTempRoundKey()
	require.NoError(h.t, err)

	initialState := &IntentSentState{
		Intents: Intents{},
	}

	errReporter := newContextErrorReporter(
		h.ctx, tempKey.LogPrefix(),
	)
	fsmCfg := ClientStateMachineCfg{
		Logger:        h.actor.log.WithPrefix(tempKey.LogPrefix()),
		ErrorReporter: errReporter,
		InitialState:  initialState,
		Env:           h.actor.env,
	}
	newFSM := protofsm.NewStateMachine(fsmCfg)
	newFSM.Start(h.ctx)

	keyStr := RoundKeyStr(tempKey.KeyString())
	h.actor.rounds[keyStr] = &RoundFSM{
		FSM: &newFSM,
		Key: tempKey,
	}

	return tempKey
}

// injectRoundInState creates a round FSM in the given initial state, keyed by
// the supplied server-assigned RoundID, and adds it to the actor's rounds map.
// It mirrors the construction of the other setupRoundIn* helpers but lets the
// caller pick the state, which the BoardingFailed routing test needs to stage
// both a lingering terminal round and a live waiting round under distinct
// RoundIDs.
func (h *actorTestHarness) injectRoundInState(roundID RoundID,
	initialState ClientState) {

	h.t.Helper()

	errReporter := newContextErrorReporter(
		h.ctx, roundID.LogPrefix(),
	)
	fsmCfg := ClientStateMachineCfg{
		Logger:        h.actor.log.WithPrefix(roundID.LogPrefix()),
		ErrorReporter: errReporter,
		InitialState:  initialState,
		Env:           h.actor.env,
	}
	newFSM := protofsm.NewStateMachine(fsmCfg)
	newFSM.Start(h.ctx)

	keyStr := RoundKeyStr(roundID.KeyString())
	h.actor.rounds[keyStr] = &RoundFSM{
		FSM:     &newFSM,
		Key:     roundID,
		RoundID: roundID,
	}
}

// setupMockRoundStoreForRecovery configures the RoundStore mock to return
// active rounds for recovery on Start(), using PartialSigsSentState which is
// stable and won't immediately transition on recovery.
func (h *actorTestHarness) setupMockRoundStoreForRecovery(rounds []*Round) {
	h.t.Helper()

	h.roundStore.On("ListActiveRounds", mock.Anything).Return(rounds, nil)

	for _, round := range rounds {
		h.roundStore.On(
			"FetchState", mock.Anything, round.RoundID,
		).Return(
			round,
			&PartialSigsSentState{RoundID: round.RoundID},
			nil,
		)
	}
}

// unknownClientMsg is a test message type not handled by the actor, used to
// test error handling for unrecognized message types.
type unknownClientMsg struct {
	actor.BaseMessage
}

func (m *unknownClientMsg) MessageType() string { return "UnknownClientMsg" }
func (m *unknownClientMsg) RoundReceivable()    {}
