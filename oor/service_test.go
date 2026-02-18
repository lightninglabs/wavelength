package oor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// testIncomingSource is an in-memory incoming event source used by service
// tests.
type testIncomingSource struct {
	mu     sync.Mutex
	events map[string][]*IncomingRecipientEvent
}

// ListRecipientEvents returns events after a cursor in ascending event-id
// order.
func (s *testIncomingSource) ListRecipientEvents(_ context.Context,
	recipientPkScript []byte, afterEventID int64,
	limit int32) ([]*IncomingRecipientEvent, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	items := s.events[string(recipientPkScript)]
	out := make([]*IncomingRecipientEvent, 0)

	for i := range items {
		if items[i].EventID <= afterEventID {
			continue
		}

		out = append(out, items[i])
		if int32(len(out)) == limit {
			break
		}
	}

	return out, nil
}

// testIncomingCursorStore stores scripts/cursors in memory for service tests.
type testIncomingCursorStore struct {
	mu      sync.Mutex
	scripts []OwnedReceiveScript
	cursors map[string]RecipientCursor
}

// newTestIncomingCursorStore constructs an in-memory cursor store.
func newTestIncomingCursorStore(
	scripts []OwnedReceiveScript) *testIncomingCursorStore {

	return &testIncomingCursorStore{
		scripts: scripts,
		cursors: make(map[string]RecipientCursor),
	}
}

// ListOwnedReceiveScripts returns the configured script set.
func (s *testIncomingCursorStore) ListOwnedReceiveScripts(_ context.Context) (
	[]OwnedReceiveScript, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]OwnedReceiveScript, 0, len(s.scripts))
	out = append(out, s.scripts...)

	return out, nil
}

// GetRecipientCursor returns the current cursor for one script.
func (s *testIncomingCursorStore) GetRecipientCursor(_ context.Context,
	recipientPkScript []byte) (*RecipientCursor, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	cursor, ok := s.cursors[string(recipientPkScript)]
	if !ok {
		return nil, nil
	}

	copyCursor := cursor

	return &copyCursor, nil
}

// UpsertRecipientCursor updates one script cursor row.
func (s *testIncomingCursorStore) UpsertRecipientCursor(_ context.Context,
	recipientPkScript []byte, lastEventID int64,
	lastSessionID *SessionID) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	var session *SessionID
	if lastSessionID != nil {
		copySession := *lastSessionID
		session = &copySession
	}

	s.cursors[string(recipientPkScript)] = RecipientCursor{
		RecipientPkScript: recipientPkScript,
		LastEventID:       lastEventID,
		LastSessionID:     session,
	}

	return nil
}

// cursor returns one stored cursor by script.
func (s *testIncomingCursorStore) cursor(
	recipientPkScript []byte) (*RecipientCursor, bool) {

	s.mu.Lock()
	defer s.mu.Unlock()

	cursor, ok := s.cursors[string(recipientPkScript)]
	if !ok {
		return nil, false
	}

	copyCursor := cursor

	return &copyCursor, true
}

// testUnrollResolver is a deterministic resolver stub for service tests.
type testUnrollResolver struct {
	wantOutpoint wire.OutPoint
	result       *db.OORUnrollPackages
}

// ResolveUnrollPackages checks the target outpoint and returns the fixture.
func (r *testUnrollResolver) ResolveUnrollPackages(_ context.Context,
	outpoint wire.OutPoint) (*db.OORUnrollPackages, error) {

	if outpoint != r.wantOutpoint {
		return nil, fmt.Errorf("unexpected outpoint")
	}

	return r.result, nil
}

// testAckFailOutboxHandler fails incoming ack requests and passes through other
// outbox messages.
type testAckFailOutboxHandler struct{}

// Handle fails only SendIncomingAckRequest to test cursor ordering guarantees.
func (h *testAckFailOutboxHandler) Handle(_ context.Context,
	_ SessionID, outbox OutboxEvent) ([]Event, error) {

	if _, ok := outbox.(*SendIncomingAckRequest); ok {
		return nil, fmt.Errorf("ack failed")
	}

	return nil, nil
}

// testAckFailOnceOutboxHandler fails the first incoming ack request and
// succeeds afterwards.
type testAckFailOnceOutboxHandler struct {
	mu       sync.Mutex
	failed   bool
	ackCalls int
}

// Handle fails the first SendIncomingAckRequest and succeeds on later calls.
func (h *testAckFailOnceOutboxHandler) Handle(_ context.Context,
	_ SessionID, outbox OutboxEvent) ([]Event, error) {

	if _, ok := outbox.(*SendIncomingAckRequest); !ok {
		return nil, nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.ackCalls++
	if h.failed {
		return nil, nil
	}

	h.failed = true

	return nil, fmt.Errorf("ack failed once")
}

// newTestServiceConfig builds a service config fixture with injectable source,
// cursor store, and transport handler.
func newTestServiceConfig(t *testing.T, operatorKey *btcec.PublicKey,
	recipientKey *btcec.PrivateKey, source IncomingEventSource,
	cursors IncomingCursorStore, transport OutboxHandler,
	packageStore PackagePersistence, vtxoStore *testVTXOStore,
	resolver UnrollPackageResolver) ServiceConfig {

	t.Helper()

	return ServiceConfig{
		ActorID:                fmt.Sprintf("oor-service-%s", t.Name()),
		DeliveryStore:          newTestDeliveryStore(t),
		TransportOutboxHandler: transport,
		VTXOStore:              vtxoStore,
		PackageStore:           packageStore,
		OperatorKey:            operatorKey,
		ExitDelay:              10,
		IncomingSource:         source,
		IncomingCursorStore:    cursors,
		IncomingPageSize:       20,
		IncomingPollInterval:   5 * time.Millisecond,
		IncomingPollJitter:     0,
		UnrollResolver:         resolver,
		ResolveIncomingClientKey: func(context.Context,
			ArkRecipientOutput) (keychain.KeyDescriptor, error) {

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(context.Context, SessionID,
			ArkRecipientOutput, *psbt.Packet,
			[]*psbt.Packet) (IncomingVTXOMetadata, error) {

			return IncomingVTXOMetadata{
				RoundID:        "round-service-test",
				CommitmentTxID: [32]byte{0x22},
				BatchExpiry:    100,
				TreeDepth:      1,
				CreatedHeight:  1,
			}, nil
		},
	}
}

// registerOutgoingActorInSystem registers a test OOR outgoing actor behind the
// service key expected by ServiceConfig.
func registerOutgoingActorInSystem(t *testing.T, system *actor.ActorSystem,
	cfg ServiceConfig) {

	t.Helper()

	localHandler := &LocalPersistenceOutboxHandler{
		Next:                     cfg.TransportOutboxHandler,
		Store:                    cfg.VTXOStore,
		PackageStore:             cfg.PackageStore,
		OperatorKey:              cfg.OperatorKey,
		ExitDelay:                cfg.ExitDelay,
		ResolveIncomingClientKey: cfg.ResolveIncomingClientKey,
		ResolveIncomingMetadata:  cfg.ResolveIncomingMetadata,
	}

	actorID := cfg.ActorID
	if actorID == "" {
		actorID = DefaultActorServiceKeyName
	}

	outgoingActor := NewOORClientActor(ClientActorCfg{
		ActorID:       actorID,
		DeliveryStore: cfg.DeliveryStore,
		OutboxHandler: localHandler,
		PackageStore:  cfg.PackageStore,
	})
	require.NoError(t, outgoingActor.startupErr)

	serviceKey := ActorServiceKey(actorID)
	bridge := actor.NewFunctionBehavior(
		func(ctx context.Context, msg ActorMsg) fn.Result[ActorResp] {
			return outgoingActor.Receive(ctx, msg)
		},
	)
	serviceKey.Spawn(system, actorID+"-bridge", bridge)

	t.Cleanup(func() {
		outgoingActor.Stop()
	})
}

// TestOORServiceOutgoingFlow verifies outgoing start/state APIs through the
// service facade.
func TestOORServiceOutgoingFlow(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	signer := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorPriv}, nil,
	)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorPriv.PubKey(),
		CSVDelay:    10,
	}

	inputAmount := btcutil.Amount(10_000)
	inputs := []TransferInput{
		newTestTransferInput(t, clientKey, operatorPriv.PubKey(),
			wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			}, inputAmount),
	}

	recipients := []oortx.RecipientOutput{{
		PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
		Value:    inputAmount,
	}}

	source := &testIncomingSource{}
	cursors := newTestIncomingCursorStore(nil)
	vtxoStore := newTestVTXOStore()
	for i := range inputs {
		require.NoError(t, vtxoStore.SaveVTXO(ctx, inputs[i].VTXO))
	}

	cfg := newTestServiceConfig(t, operatorPriv.PubKey(), clientKey,
		source, cursors, &testOutboxHandler{
			t:              t,
			clientSigner:   signer,
			operatorSigner: operatorSigner,
		}, &testPackageStore{}, vtxoStore, nil)

	svcIface, err := NewOORService(cfg)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, svcIface.Stop(t.Context()))
	}()

	svc, ok := svcIface.(*oorService)
	require.True(t, ok)

	sessionID, err := svc.StartOutgoing(ctx, StartOutgoingRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.NoError(t, err)
	require.NotEqual(t, SessionID{}, sessionID)

	view, err := svc.GetOutgoingState(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionID, view.SessionID)
	require.Equal(t, "Completed", view.StateName)
	require.True(t, view.Terminal)
}

// TestOORServiceSyncIncomingOnce verifies one incoming cycle processes events
// and advances recipient cursors only after successful ack.
func TestOORServiceSyncIncomingOnce(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkPSBT, checkpoints, recipients, _, recipientKey, operatorKey :=
		buildTestIncomingMaterialization(t)
	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())

	recipientScript := recipients[0].PkScript
	event := &IncomingRecipientEvent{
		EventID:              10,
		SessionID:            sessionID,
		RecipientPkScript:    recipientScript,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: checkpoints,
		CreatedAt:            time.Now(),
	}

	source := &testIncomingSource{
		events: map[string][]*IncomingRecipientEvent{
			string(recipientScript): {event},
		},
	}

	cursors := newTestIncomingCursorStore([]OwnedReceiveScript{{
		PkScript: recipientScript,
	}})

	packageStore := &testPackageStore{}
	vtxoStore := newTestVTXOStore()
	cfg := newTestServiceConfig(t, operatorKey, recipientKey,
		source, cursors, nil, packageStore, vtxoStore, nil)

	svcIface, err := NewOORService(cfg)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, svcIface.Stop(t.Context()))
	}()

	err = svcIface.SyncIncomingOnce(ctx)
	require.NoError(t, err)

	cursor, ok := cursors.cursor(recipientScript)
	require.True(t, ok)
	require.Equal(t, int64(10), cursor.LastEventID)
	require.NotNil(t, cursor.LastSessionID)
	require.Equal(t, sessionID, *cursor.LastSessionID)

	outpoint := wire.OutPoint{
		Hash:  arkPSBT.UnsignedTx.TxHash(),
		Index: recipients[0].OutputIndex,
	}

	desc, err := vtxoStore.GetVTXO(ctx, outpoint)
	require.NoError(t, err)
	require.Equal(t, recipients[0].Value, desc.Amount)

	require.Equal(t, 1, packageStore.packageCalls)
	require.Equal(t, 1, packageStore.bindingCalls)

	status := svcIface.GetIncomingSyncStatus()
	require.Equal(t, 1, status.LastRunProcessedScripts)
	require.Equal(t, 1, status.LastRunProcessedEvents)
	require.Equal(t, int64(1), status.TotalProcessedEvents)
	require.Empty(t, status.LastError)
}

// TestOORServiceSyncIncomingAckFailureDoesNotAdvanceCursor verifies a failing
// ack path leaves cursors unchanged.
func TestOORServiceSyncIncomingAckFailureDoesNotAdvanceCursor(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkPSBT, checkpoints, recipients, _, recipientKey, operatorKey :=
		buildTestIncomingMaterialization(t)
	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())

	recipientScript := recipients[0].PkScript
	event := &IncomingRecipientEvent{
		EventID:              20,
		SessionID:            sessionID,
		RecipientPkScript:    recipientScript,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: checkpoints,
	}

	source := &testIncomingSource{
		events: map[string][]*IncomingRecipientEvent{
			string(recipientScript): {event},
		},
	}

	cursors := newTestIncomingCursorStore([]OwnedReceiveScript{{
		PkScript: recipientScript,
	}})

	cfg := newTestServiceConfig(t, operatorKey, recipientKey,
		source, cursors, &testAckFailOutboxHandler{},
		&testPackageStore{}, newTestVTXOStore(), nil)

	svcIface, err := NewOORService(cfg)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, svcIface.Stop(t.Context()))
	}()

	err = svcIface.SyncIncomingOnce(ctx)
	require.Error(t, err)

	_, ok := cursors.cursor(recipientScript)
	require.False(t, ok)
}

// TestOORServiceSyncIncomingAckRetryIsIdempotent verifies incoming replay is
// safe when a cycle fails after materialization but before cursor persistence.
func TestOORServiceSyncIncomingAckRetryIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkPSBT, checkpoints, recipients, _, recipientKey, operatorKey :=
		buildTestIncomingMaterialization(t)
	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())

	recipientScript := recipients[0].PkScript
	event := &IncomingRecipientEvent{
		EventID:              30,
		SessionID:            sessionID,
		RecipientPkScript:    recipientScript,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: checkpoints,
	}

	source := &testIncomingSource{
		events: map[string][]*IncomingRecipientEvent{
			string(recipientScript): {event},
		},
	}

	cursors := newTestIncomingCursorStore([]OwnedReceiveScript{{
		PkScript: recipientScript,
	}})
	transport := &testAckFailOnceOutboxHandler{}
	vtxoStore := newTestVTXOStore()

	cfg := newTestServiceConfig(t, operatorKey, recipientKey,
		source, cursors, transport, &testPackageStore{}, vtxoStore, nil)

	svcIface, err := NewOORService(cfg)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, svcIface.Stop(t.Context()))
	}()

	err = svcIface.SyncIncomingOnce(ctx)
	require.ErrorContains(t, err, "ack failed once")

	_, ok := cursors.cursor(recipientScript)
	require.False(t, ok)

	err = svcIface.SyncIncomingOnce(ctx)
	require.NoError(t, err)

	cursor, ok := cursors.cursor(recipientScript)
	require.True(t, ok)
	require.Equal(t, int64(30), cursor.LastEventID)
	require.NotNil(t, cursor.LastSessionID)
	require.Equal(t, sessionID, *cursor.LastSessionID)

	live, err := vtxoStore.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, live, 1)

	transport.mu.Lock()
	require.Equal(t, 2, transport.ackCalls)
	transport.mu.Unlock()
}

// TestOORServiceOutgoingFlowViaActorSystem verifies outgoing orchestration can
// route through actor-system service-key lookup instead of direct actor calls.
func TestOORServiceOutgoingFlowViaActorSystem(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	signer := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorPriv}, nil,
	)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorPriv.PubKey(),
		CSVDelay:    10,
	}

	inputAmount := btcutil.Amount(10_000)
	inputs := []TransferInput{
		newTestTransferInput(t, clientKey, operatorPriv.PubKey(),
			wire.OutPoint{
				Hash:  [32]byte{0x0A},
				Index: 0,
			}, inputAmount),
	}

	recipients := []oortx.RecipientOutput{{
		PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
		Value:    inputAmount,
	}}

	source := &testIncomingSource{}
	cursors := newTestIncomingCursorStore(nil)
	vtxoStore := newTestVTXOStore()
	for i := range inputs {
		require.NoError(t, vtxoStore.SaveVTXO(ctx, inputs[i].VTXO))
	}

	cfg := newTestServiceConfig(t, operatorPriv.PubKey(), clientKey,
		source, cursors, &testOutboxHandler{
			t:              t,
			clientSigner:   signer,
			operatorSigner: operatorSigner,
		}, &testPackageStore{}, vtxoStore, nil)

	actorSystem := actor.NewActorSystem()
	t.Cleanup(func() {
		baseCtx := context.WithoutCancel(t.Context())
		shutdownCtx, cancel := context.WithTimeout(
			baseCtx, time.Second,
		)
		defer cancel()

		require.NoError(t, actorSystem.Shutdown(shutdownCtx))
	})

	registerOutgoingActorInSystem(t, actorSystem, cfg)
	cfg.ActorSystem = actorSystem
	cfg.DeliveryStore = nil

	svcIface, err := NewOORService(cfg)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, svcIface.Stop(t.Context()))
	}()

	sessionID, err := svcIface.StartOutgoing(ctx, StartOutgoingRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.NoError(t, err)
	require.NotEqual(t, SessionID{}, sessionID)

	view, err := svcIface.GetOutgoingState(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionID, view.SessionID)
	require.Equal(t, "Completed", view.StateName)
	require.True(t, view.Terminal)
}

// TestNewOORServiceActorSystemRequiresOutgoingActor verifies constructor
// validation when actor-system lookup is enabled without a registered actor.
func TestNewOORServiceActorSystemRequiresOutgoingActor(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	cfg := newTestServiceConfig(t, operatorPriv.PubKey(), recipientKey,
		&testIncomingSource{}, newTestIncomingCursorStore(nil), nil,
		&testPackageStore{}, newTestVTXOStore(), nil)

	actorSystem := actor.NewActorSystem()
	t.Cleanup(func() {
		baseCtx := context.WithoutCancel(t.Context())
		shutdownCtx, cancel := context.WithTimeout(
			baseCtx, time.Second,
		)
		defer cancel()

		require.NoError(t, actorSystem.Shutdown(shutdownCtx))
	})

	cfg.ActorSystem = actorSystem
	cfg.DeliveryStore = nil

	_, err = NewOORService(cfg)
	require.ErrorContains(t, err, "no outgoing actor registered")
}

// TestOORServiceResolveUnrollPackages verifies resolver passthrough.
func TestOORServiceResolveUnrollPackages(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{0x55},
		Index: 3,
	}

	result := &db.OORUnrollPackages{TargetOutpoint: outpoint}
	resolver := &testUnrollResolver{
		wantOutpoint: outpoint,
		result:       result,
	}

	cfg := newTestServiceConfig(t, operatorPriv.PubKey(), recipientKey,
		&testIncomingSource{}, newTestIncomingCursorStore(nil), nil,
		&testPackageStore{}, newTestVTXOStore(), resolver)

	svcIface, err := NewOORService(cfg)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, svcIface.Stop(t.Context()))
	}()

	resolved, err := svcIface.ResolveUnrollPackages(ctx, outpoint)
	require.NoError(t, err)
	require.Equal(t, result, resolved)
}

// TestOORServiceIncomingWorkerLifecycle verifies the background worker can be
// started and stopped cleanly.
func TestOORServiceIncomingWorkerLifecycle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	cfg := newTestServiceConfig(t, operatorPriv.PubKey(), recipientKey,
		&testIncomingSource{}, newTestIncomingCursorStore(nil), nil,
		&testPackageStore{}, newTestVTXOStore(), nil)

	svcIface, err := NewOORService(cfg)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, svcIface.Stop(t.Context()))
	}()

	err = svcIface.StartIncomingSync(ctx)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return svcIface.GetIncomingSyncStatus().Running
	}, time.Second, 10*time.Millisecond)

	err = svcIface.StopIncomingSync(ctx)
	require.NoError(t, err)

	require.False(t, svcIface.GetIncomingSyncStatus().Running)
}

var _ IncomingEventSource = (*testIncomingSource)(nil)
var _ IncomingCursorStore = (*testIncomingCursorStore)(nil)
var _ UnrollPackageResolver = (*testUnrollResolver)(nil)
var _ OutboxHandler = (*testAckFailOutboxHandler)(nil)
var _ OutboxHandler = (*testAckFailOnceOutboxHandler)(nil)
