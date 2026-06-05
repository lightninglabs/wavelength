package oor

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	clientdb "github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// fakeRegistryStore is an in-memory SessionRegistryStore for unit tests.
type fakeRegistryStore struct {
	rows map[chainhash.Hash]clientdb.OORSessionRegistryRecord
}

func newFakeRegistryStore() *fakeRegistryStore {
	return &fakeRegistryStore{
		rows: make(
			map[chainhash.Hash]clientdb.OORSessionRegistryRecord,
		),
	}
}

func (s *fakeRegistryStore) UpsertSession(_ context.Context,
	record clientdb.OORSessionRegistryRecord) error {

	s.rows[record.SessionID] = record

	return nil
}

func (s *fakeRegistryStore) GetSession(_ context.Context,
	sessionID chainhash.Hash) (*clientdb.OORSessionRegistryRecord, error) {

	record, ok := s.rows[sessionID]
	if !ok {
		return nil, clientdb.ErrOORSessionNotFound
	}

	return &record, nil
}

func (s *fakeRegistryStore) LookupActiveSessionByIdempotencyKey(
	_ context.Context, key string) (*clientdb.OORSessionRegistryRecord,
	error) {

	for i := range s.rows {
		record := s.rows[i]
		failed := record.Status == clientdb.OORSessionStatusFailed
		if record.IdempotencyKey == key && key != "" && !failed {
			return &record, nil
		}
	}

	return nil, clientdb.ErrOORSessionNotFound
}

func (s *fakeRegistryStore) ListNonTerminal(_ context.Context) (
	[]clientdb.OORSessionRegistryRecord, error) {

	var out []clientdb.OORSessionRegistryRecord
	for i := range s.rows {
		if !s.rows[i].Status.IsTerminal() {
			out = append(out, s.rows[i])
		}
	}

	return out, nil
}

func (s *fakeRegistryStore) ListSessions(_ context.Context) (
	[]clientdb.OORSessionRegistryRecord, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]clientdb.OORSessionRegistryRecord, 0, len(s.rows))
	for i := range s.rows {
		out = append(out, s.rows[i])
	}

	return out, nil
}

// fakePackageStore records finalized package and binding writes.
type fakePackageStore struct {
	packages int
	bindings int
}

func (s *fakePackageStore) UpsertPackage(_ context.Context, _ PackageDirection,
	_ chainhash.Hash, _ *psbt.Packet, _ []*psbt.Packet) error {

	s.packages++

	return nil
}

func (s *fakePackageStore) UpsertBinding(_ context.Context, _ wire.OutPoint,
	_ chainhash.Hash, _ uint32, _ PackageLinkKind) error {

	s.bindings++

	return nil
}

// finalizeBehavior builds a per-session behavior whose FSM is parked in
// AwaitingFinalizeAccepted, ready to receive a FinalizeAcceptedEvent.
func finalizeBehavior(t *testing.T, registry SessionRegistryStore,
	pkgStore PackagePersistence, completer SpendCompleter) (
	*sessionBehavior, SessionID, []wire.OutPoint) {

	t.Helper()

	ctx := t.Context()

	ark, checkpoints := testOutboxPSBTPair(t)
	sessionID, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	inputs := testRetryTransferInputs(t)
	snapshot, err := NewOutgoingSnapshot(
		sessionID, &AwaitingFinalizeAccepted{
			SessionID:            sessionID,
			ArkPSBT:              ark,
			FinalCheckpointPSBTs: checkpoints,
			TransferInputs:       inputs,
		},
	)
	require.NoError(t, err)

	session, err := NewSessionFromSnapshot(ctx, snapshot)
	require.NoError(t, err)

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore:  registry,
			PackageStore:   pkgStore,
			SpendCompleter: completer,
		},
		actorID:   ActorIDForSession(sessionID),
		log:       btclog.Disabled,
		sessionID: sessionID,
		direction: clientdb.OORSessionDirectionOutgoing,
		fsm:       session.FSM,
		loaded:    true,
	}

	return b, sessionID, InputOutpoints(inputs)
}

// TestSessionActorOutgoingFinalize verifies that a FinalizeAcceptedEvent drives
// the session to completion: the inputs are marked spent, the finalized package
// is persisted, and the snapshot reaches a completed terminal status.
func TestSessionActorOutgoingFinalize(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	var spent []wire.OutPoint
	completer := func(_ context.Context, ops []wire.OutPoint) error {
		spent = append(spent, ops...)

		return nil
	}

	registry := newFakeRegistryStore()
	pkgStore := &fakePackageStore{}
	b, sessionID, wantOutpoints := finalizeBehavior(
		t, registry, pkgStore, completer,
	)

	// Drain the FSM: FinalizeAccepted -> mark spent -> completed.
	b.pendingTransport = b.pendingTransport[:0]
	b.commitWork = b.commitWork[:0]
	b.postCommit = b.postCommit[:0]

	res := b.dispatch(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event:     &FinalizeAcceptedEvent{},
	})
	require.True(t, res.IsOk(), res.Err())

	// The FSM should now be terminal-completed.
	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &Completed{}, state)

	// Run the staged commit work against a dummy tx store and assert the
	// spend completion and package persistence both happened, in order.
	for _, work := range b.commitWork {
		require.NoError(t, work(ctx, oorTx{}))
	}

	require.ElementsMatch(t, wantOutpoints, spent)
	require.Equal(t, 1, pkgStore.packages)

	// The snapshot for the completed session must report terminal status.
	record, err := b.snapshotRecord()
	require.NoError(t, err)
	require.Equal(t, clientdb.OORSessionStatusCompleted, record.Status)
	require.Equal(t, string(OutgoingPhaseCompleted), record.Phase)
}

// TestSessionActorSnapshotRestoreRoundTrip verifies a behavior can persist its
// snapshot to the registry and a fresh behavior restores the exact same FSM
// state from that row.
func TestSessionActorSnapshotRestoreRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	registry := newFakeRegistryStore()
	b, sessionID, _ := finalizeBehavior(
		t, registry, &fakePackageStore{}, nil,
	)

	// Persist the current (AwaitingFinalizeAccepted) snapshot.
	record, err := b.snapshotRecord()
	require.NoError(t, err)
	require.NoError(t, registry.UpsertSession(ctx, record))
	require.Equal(t, string(OutgoingPhaseFinalizeSent), record.Phase)

	// A fresh behavior restores the same FSM state from the row.
	restored := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore: registry,
		},
		actorID:   ActorIDForSession(sessionID),
		log:       btclog.Disabled,
		sessionID: sessionID,
	}
	require.NoError(t, restored.restore(ctx))

	require.NotNil(t, restored.fsm)
	require.Equal(
		t, clientdb.OORSessionDirectionOutgoing, restored.direction,
	)

	state, err := restored.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &AwaitingFinalizeAccepted{}, state)
}

// TestSessionActorRestoreThenResumeReemitsTransport is the restart regression
// test for the outgoing flow: a session interrupted while awaiting the finalize
// acceptance is restored from its registry row, and the boot-time resume
// re-emits the finalize transport so the session makes forward progress instead
// of stalling on an operator response that was lost in the restart.
func TestSessionActorRestoreThenResumeReemitsTransport(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	registry := newFakeRegistryStore()
	b, sessionID, _ := finalizeBehavior(
		t, registry, &fakePackageStore{}, nil,
	)

	// Persist the in-flight (AwaitingFinalizeAccepted) snapshot, simulating
	// the state of the world at crash time.
	record, err := b.snapshotRecord()
	require.NoError(t, err)
	require.NoError(t, registry.UpsertSession(ctx, record))

	// "Restart": a fresh behavior restores from the row, exactly as
	// NewOORSessionActor does at construction.
	restored := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore: registry,
		},
		actorID:   ActorIDForSession(sessionID),
		log:       btclog.Disabled,
		sessionID: sessionID,
	}
	require.NoError(t, restored.restore(ctx))

	// The boot-time resume re-drives the outbox implied by the restored
	// state: the finalize request must be re-collected for transport.
	restored.pendingTransport = restored.pendingTransport[:0]
	restored.commitWork = restored.commitWork[:0]
	restored.postCommit = restored.postCommit[:0]

	res := restored.dispatch(ctx, &ResumeSessionRequest{
		SessionID: sessionID,
	})
	require.True(t, res.IsOk(), res.Err())

	require.Len(t, restored.pendingTransport, 1)
	transport := restored.pendingTransport[0]
	wrapped, ok := transport.(*serverconn.SendClientEventRequest)
	require.True(t, ok)
	require.IsType(t, &SendFinalizePackageRequest{}, wrapped.Message)

	// The FSM must stay parked in the awaiting state: resume re-emits
	// transport, it does not advance the session.
	state, err := restored.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &AwaitingFinalizeAccepted{}, state)
}

// TestSessionActorResumeIncomingResolving is the restart regression test for
// the incoming flow: a receive session restored in ReceiveResolving re-emits
// the phase-1 indexer query on resume.
func TestSessionActorResumeIncomingResolving(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore: newFakeRegistryStore(),
		},
		log: btclog.Disabled,
	}

	sid := oorSessionID(0x61)
	res := b.handleResolveIncomingTransfer(
		ctx, &ResolveIncomingTransferRequest{
			SessionID:         sid,
			RecipientPkScript: []byte{0x51, 0x20, 0xaa, 0xbb},
			RecipientEventID:  9,
		},
	)
	require.True(t, res.IsOk(), res.Err())

	// Drop the admission turn's collected transport, then resume: the
	// phase-1 query must be re-collected from the parked state alone.
	b.pendingTransport = b.pendingTransport[:0]
	b.commitWork = b.commitWork[:0]
	b.postCommit = b.postCommit[:0]

	res = b.dispatch(ctx, &ResumeSessionRequest{SessionID: sid})
	require.True(t, res.IsOk(), res.Err())

	require.Len(t, b.pendingTransport, 1)
	query := b.pendingTransport[0]
	_, ok := query.(*serverconn.SendListOORRecipientEventsByScriptRequest)
	require.True(t, ok)

	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &ReceiveResolving{}, state)
}

// TestSessionActorSubmitAcceptedNilArkPSBTEnrichment verifies a server-push
// SubmitAcceptedEvent that does not echo the ArkPSBT back is enriched from the
// AwaitingSubmitAccepted state, so the session advances instead of failing
// against operators that omit co_signed_ark_psbt from the response.
func TestSessionActorSubmitAcceptedNilArkPSBTEnrichment(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{clientKey}, nil,
	)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	inputValue := btcutil.Amount(10_000)
	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, policy.OperatorKey, wire.OutPoint{
				Hash:  [32]byte{0x05},
				Index: 0,
			},
			inputValue,
		),
	}
	recipients := []oortx.RecipientOutput{{
		PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
		Value:    inputValue,
	}}

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore: newFakeRegistryStore(),
			Signer:        clientSigner,
		},
		log: btclog.Disabled,
	}

	// Admission turn: builds the deterministic package, signs the Ark PSBT
	// inline, and collects the submit transport.
	res := b.dispatch(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, res.IsOk(), res.Err())

	require.Len(t, b.pendingTransport, 1)
	submitTransport := b.pendingTransport[0]
	submitWrap, ok := submitTransport.(*serverconn.SendClientEventRequest)
	require.True(t, ok)
	submitMsg, ok := submitWrap.Message.(*SendSubmitPackageRequest)
	require.True(t, ok)

	// Model the operator co-signing the checkpoints for the accepted
	// response.
	require.NoError(
		t, coSignCheckpointPSBTsForTest(
			operatorSigner, submitMsg.TransferInputs,
			submitMsg.CheckpointPSBTs,
		),
	)

	// Server-push turn: the event carries no ArkPSBT, matching operators
	// whose submit response omits co_signed_ark_psbt.
	b.pendingTransport = b.pendingTransport[:0]
	b.commitWork = b.commitWork[:0]
	b.postCommit = b.postCommit[:0]

	res = b.dispatch(ctx, &DriveEventRequest{
		SessionID: b.sessionID,
		Event: &SubmitAcceptedEvent{
			SessionID:               b.sessionID,
			ArkPSBT:                 nil,
			CoSignedCheckpointPSBTs: submitMsg.CheckpointPSBTs,
		},
	})
	require.True(t, res.IsOk(), res.Err())

	// Enrichment plus inline checkpoint signing advance the session to
	// AwaitingFinalizeAccepted with the canonical ArkPSBT carried over.
	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	finalize, ok := state.(*AwaitingFinalizeAccepted)
	require.True(t, ok, "expected AwaitingFinalizeAccepted, got %T", state)
	require.NotNil(t, finalize.ArkPSBT)

	require.Len(t, b.pendingTransport, 1)
	finalizeTransport := b.pendingTransport[0]
	finalizeWrap, ok :=
		finalizeTransport.(*serverconn.SendClientEventRequest)
	require.True(t, ok)
	require.IsType(t, &SendFinalizePackageRequest{}, finalizeWrap.Message)
}

// fakeExec runs every Read/Stage/Commit closure inline against the carried
// transaction store, standing in for the durable framework in behavior tests.
type fakeExec struct {
	tx oorTx
}

func (e fakeExec) Read(ctx context.Context,
	fn func(context.Context, oorTx) error) error {

	return fn(ctx, e.tx)
}

func (e fakeExec) Stage(ctx context.Context,
	fn func(context.Context, oorTx) error) error {

	return fn(ctx, e.tx)
}

func (e fakeExec) Commit(ctx context.Context,
	fn func(context.Context, oorTx) error) error {

	return fn(ctx, e.tx)
}

// fakeDeliveryStore records durable outbox enqueues; every other DeliveryStore
// method panics via the embedded nil interface.
type fakeDeliveryStore struct {
	actor.DeliveryStore

	enqueued []actor.OutboxParams
}

func (s *fakeDeliveryStore) EnqueueOutbox(_ context.Context,
	params actor.OutboxParams) error {

	s.enqueued = append(s.enqueued, params)

	return nil
}

// fakeServerConnRef is a no-op serverconn tell target for behavior tests.
type fakeServerConnRef struct{}

func (fakeServerConnRef) ID() string {
	return "serverconn"
}

func (fakeServerConnRef) Tell(context.Context, serverconn.ServerConnMsg) error {
	return nil
}

// TestSessionActorTerminalCommitNotifiesRegistry verifies a turn that commits
// a terminal snapshot notifies the registry exactly once so the child can be
// reaped, while non-terminal turns stay silent.
func TestSessionActorTerminalCommitNotifiesRegistry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	rec := &registrySpawnRecorder{}
	registryRef := &recordingTellRef{id: "oor-registry", rec: rec}

	store := newFakeRegistryStore()
	b, sessionID, _ := finalizeBehavior(
		t, store, &fakePackageStore{},
		func(context.Context, []wire.OutPoint) error { return nil },
	)
	b.cfg.Registry = registryRef
	b.cfg.ServerConn = fakeServerConnRef{}

	ax := fakeExec{tx: oorTx{
		store:   &fakeDeliveryStore{},
		actorID: b.actorID,
	}}

	// A resume turn re-emits transport but stays in the awaiting state:
	// no terminal notification may fire.
	res := b.Receive(ctx, &ResumeSessionRequest{
		SessionID: sessionID,
	}, ax)
	require.True(t, res.IsOk(), res.Err())
	require.False(t, b.terminalCommitted)

	// The finalize acceptance drives the session to Completed; the commit
	// is terminal and the registry must hear about it.
	res = b.Receive(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event:     &FinalizeAcceptedEvent{},
	}, ax)
	require.True(t, res.IsOk(), res.Err())
	require.True(t, b.terminalCommitted)

	// The notification is fired asynchronously so a full registry mailbox
	// can never wedge the child's turn.
	require.Eventually(t, func() bool {
		msgs := rec.recorded()
		if len(msgs) != 1 {
			return false
		}

		notify, ok := msgs[0].(*SessionTerminalNotification)

		return ok && notify.SessionID == sessionID
	}, time.Second, 10*time.Millisecond)
}

// TestSessionActorRestoreDirectionMismatch verifies restore refuses to
// silently adopt a row whose direction conflicts with the requested one,
// except for the self-transfer replacement (incoming requested over a terminal
// outgoing row), which leaves the behavior unloaded for a fresh admission.
func TestSessionActorRestoreDirectionMismatch(t *testing.T) {
	t.Parallel()

	id := oorSessionID(0x71)

	testCases := []struct {
		name      string
		requested clientdb.OORSessionDirection
		rowDir    clientdb.OORSessionDirection
		rowStatus clientdb.OORSessionStatus
		wantErr   string

		// wantLoaded reports whether restore must end with a live FSM.
		wantLoaded bool
	}{{
		name:      "incoming over live outgoing row errors",
		requested: clientdb.OORSessionDirectionIncoming,
		rowDir:    clientdb.OORSessionDirectionOutgoing,
		rowStatus: clientdb.OORSessionStatusPending,
		wantErr:   "direction mismatch",
	}, {
		name:      "outgoing over incoming row errors",
		requested: clientdb.OORSessionDirectionOutgoing,
		rowDir:    clientdb.OORSessionDirectionIncoming,
		rowStatus: clientdb.OORSessionStatusPending,
		wantErr:   "direction mismatch",
	}, {
		// The self-transfer replacement: the behavior stays unloaded
		// so the admission installs a fresh incoming session.
		name:      "incoming over terminal outgoing row is fresh",
		requested: clientdb.OORSessionDirectionIncoming,
		rowDir:    clientdb.OORSessionDirectionOutgoing,
		rowStatus: clientdb.OORSessionStatusCompleted,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := newFakeRegistryStore()
			upsertRegistryRow(
				t, store, id, tc.rowDir, "submit_sent", "",
				tc.rowStatus,
			)

			b := &sessionBehavior{
				cfg: SessionActorConfig{
					RegistryStore: store,
				},
				actorID:   ActorIDForSession(id),
				log:       btclog.Disabled,
				sessionID: id,
				direction: tc.requested,
			}

			err := b.restore(t.Context())
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantLoaded, b.loaded)
			require.Nil(t, b.fsm)
		})
	}
}
