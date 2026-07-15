package oor

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/rpc/oorpb"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestSessionRestoreRejectsUnknownFlowVersion proves the OOR flow-version guard
// fires in the real restore path: a session whose persisted registry row
// carries a flow version this build does not understand (V2) is rejected before
// any FSM reconstruction rather than silently resumed.
func TestSessionRestoreRejectsUnknownFlowVersion(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newFakeRegistryStore()
	id := oorSessionID(0x07)

	require.NoError(t, store.UpsertSession(ctx,
		clientdb.OORSessionRegistryRecord{
			SessionID:    chainHashOf(id),
			ActorID:      ActorIDForSession(id),
			Direction:    clientdb.OORSessionDirectionOutgoing,
			Phase:        "submit_sent",
			Status:       clientdb.OORSessionStatusPending,
			SnapshotData: []byte{0x01},

			// A flow version from a hypothetical future build.
			FlowVersion: oorpb.FlowVersionV1 + 1,
		},
	))

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore: store,
		},
		actorID:   ActorIDForSession(id),
		log:       btclog.Disabled,
		sessionID: id,
		direction: clientdb.OORSessionDirectionOutgoing,
	}

	err := b.restore(ctx)
	require.ErrorContains(t, err, "unknown OOR flow version")
}

// fakeIncomingHandler answers the materialization request with a fixed
// IncomingHandledEvent, standing in for the local-persistence handler.
type fakeIncomingHandler struct {
	descs []*vtxo.Descriptor
	err   error
}

func (h *fakeIncomingHandler) Handle(_ context.Context, _ SessionID,
	outbox OutboxEvent) ([]Event, error) {

	if h.err != nil {
		return nil, h.err
	}

	if _, ok := outbox.(*MaterializeIncomingVTXOsRequest); !ok {
		return nil, fmt.Errorf("unexpected outbox event %T", outbox)
	}

	return []Event{&IncomingHandledEvent{
		MaterializedVTXOs: h.descs,
	}}, nil
}

// recordingManagerRef records VTXO manager Tells from the session actor.
type recordingManagerRef struct {
	mu   sync.Mutex
	msgs []vtxo.ManagerMsg
}

func (r *recordingManagerRef) ID() string {
	return "vtxo-manager"
}

func (r *recordingManagerRef) Tell(_ context.Context,
	msg vtxo.ManagerMsg) error {

	r.mu.Lock()
	defer r.mu.Unlock()

	r.msgs = append(r.msgs, msg)

	return nil
}

func (r *recordingManagerRef) recorded() []vtxo.ManagerMsg {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]vtxo.ManagerMsg(nil), r.msgs...)
}

// TestSessionActorIncomingMaterializeFullFlow drives a full incoming
// materialization turn: the metadata resolution stages the materialize work
// into the commit, the handler's IncomingHandledEvent advances the FSM to
// completion via the ack, the ledger receive entries are told inside the
// commit transaction, and the manager/observer notifications fire after it.
func TestSessionActorIncomingMaterializeFullFlow(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	ark, checkpoints := testOutboxPSBTPair(t)
	sid := oorSessionID(0x71)
	session, err := newReceiveSessionWithState(
		ctx, sid, &ReceiveNotified{
			SessionID:            sid,
			ArkPSBT:              ark,
			FinalCheckpointPSBTs: checkpoints,
		},
		EnvConfig{},
	)
	require.NoError(t, err)

	descs := []*vtxo.Descriptor{
		{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					0x71,
				},
				Index: 1,
			},
			Amount: btcutil.Amount(5_000),
		},
		{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					0x71,
				},
				Index: 2,
			},
			Amount: btcutil.Amount(7_000),
		},
	}

	manager := &recordingManagerRef{}

	// The post-commit notifications run on their own goroutine with a
	// daemon-owned context, so the observer must be safe for concurrent
	// append and the assertions below poll for delivery.
	var (
		observedMu sync.Mutex
		observed   []*vtxo.Descriptor
	)
	var inCommit bool
	sink := &recordingLedgerSink{inCommit: &inCommit}
	delivery := &fakeDeliveryStore{}
	conn := &recordingServerConnRef{}

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore: newFakeRegistryStore(),
			IncomingHandler: &fakeIncomingHandler{
				descs: descs,
			},
			VTXOManager: manager,
			IncomingVTXOObserver: func(_ context.Context,
				d []*vtxo.Descriptor) error {

				observedMu.Lock()
				defer observedMu.Unlock()
				observed = append(observed, d...)

				return nil
			},
			LedgerSink: fn.Some[ledger.Sink](sink),
			ServerConn: conn,
		},
		actorID:   ActorIDForSession(sid),
		log:       btclog.Disabled,
		sessionID: sid,
		direction: clientdb.OORSessionDirectionIncoming,
		fsm:       session.FSM,
		loaded:    true,
	}

	ax := commitTrackingExec{
		fakeExec: fakeExec{tx: oorTx{
			store:   delivery,
			actorID: b.actorID,
		}},
		inCommit: &inCommit,
	}

	res := b.Receive(ctx, &DriveEventRequest{
		SessionID: sid,
		Event:     &IncomingMetadataResolvedEvent{},
	}, ax)
	require.True(t, res.IsOk(), res.Err())

	// The FSM completed and the commit carried the ack transport plus the
	// terminal snapshot.
	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &ReceiveCompleted{}, state)
	require.True(t, b.terminalCommitted)

	// The ack transport was Tell'd directly into the serverconn durable
	// actor during the commit (no generic outbox enqueue).
	require.Empty(t, delivery.enqueued)
	require.Len(t, conn.recorded(), 1)

	// One ledger receive entry per descriptor, told inside the commit.
	require.Len(t, sink.msgs, len(descs))
	require.Equal(t, []bool{true, true}, sink.toldInCommit)
	for i, msg := range sink.msgs {
		recv, ok := msg.(*ledger.VTXOReceivedMsg)
		require.True(t, ok, "expected VTXOReceivedMsg, got %T", msg)
		require.Equal(t, int64(descs[i].Amount), recv.AmountSat)
		require.Equal(t, ledger.SourceOOR, recv.Source)
		require.Equal(
			t, descs[i].Outpoint.Hash,
			chainhash.Hash(recv.OutpointHash),
		)
	}

	// The post-commit best-effort notifications fire for the VTXO manager
	// and the fraud observer with the full descriptor set. They are
	// delivered asynchronously on a daemon-owned context (so a terminal
	// turn's cancellation cannot drop them), hence the poll.
	require.Eventually(t, func() bool {
		observedMu.Lock()
		defer observedMu.Unlock()

		return len(manager.recorded()) == 1 &&
			len(observed) == len(descs)
	}, 5*time.Second, 10*time.Millisecond)

	mgrMsgs := manager.recorded()
	notify, ok := mgrMsgs[0].(*vtxo.VTXOsMaterializedNotification)
	require.True(t, ok)
	require.Len(t, notify.VTXOs, len(descs))
}

// countingIncomingHandler is a fakeIncomingHandler that records how many times
// Handle was invoked, so a test can assert materialization re-ran after a
// rolled-back commit.
type countingIncomingHandler struct {
	fakeIncomingHandler

	calls int
}

func (h *countingIncomingHandler) Handle(ctx context.Context, sid SessionID,
	outbox OutboxEvent) ([]Event, error) {

	h.calls++

	return h.fakeIncomingHandler.Handle(ctx, sid, outbox)
}

// failFirstUpsertStore seeds a last-committed row and fails the first
// UpsertSession so the first commit rolls back, then succeeds afterwards. It
// models the durable store across a turn whose Commit aborts after the
// in-memory FSM already advanced.
type failFirstUpsertStore struct {
	*fakeRegistryStore

	failsLeft int
}

func (s *failFirstUpsertStore) UpsertSession(ctx context.Context,
	record clientdb.OORSessionRegistryRecord) error {

	if s.failsLeft > 0 {
		s.failsLeft--

		return errFilterBroken
	}

	return s.fakeRegistryStore.UpsertSession(ctx, record)
}

// TestSessionActorIncomingReloadsAfterFailedCommit verifies the critical
// invariant that the in-memory FSM is never observably ahead of the
// last-committed snapshot. An incoming materialization turn advances the FSM to
// ReceiveCompleted inside its commit closure; when that commit rolls back, the
// redelivered driving event must re-run materialization against the
// last-committed ReceiveNotified state instead of being discarded by a
// terminal in-memory FSM (which would persist a Completed session whose VTXOs
// were never materialized).
func TestSessionActorIncomingReloadsAfterFailedCommit(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	ark, checkpoints := testOutboxPSBTPair(t)

	// Restore re-validates that the session id matches the Ark txid, so the
	// session id is derived from the Ark rather than a synthetic byte.
	sid, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	// Seed the last durably-committed row at ReceiveNotified: this is the
	// state restore must rebuild the FSM to after the failed commit.
	notified := &ReceiveNotified{
		SessionID:            sid,
		ArkPSBT:              ark,
		FinalCheckpointPSBTs: checkpoints,
	}
	notifiedRecord, err := incomingRegistryRecord(sid, notified)
	require.NoError(t, err)

	base := newFakeRegistryStore()
	require.NoError(t, base.UpsertSession(ctx, notifiedRecord))
	store := &failFirstUpsertStore{
		fakeRegistryStore: base,
		failsLeft:         1,
	}

	descs := []*vtxo.Descriptor{{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x74,
			},
			Index: 1,
		},
		Amount: btcutil.Amount(9_000),
	}}
	handler := &countingIncomingHandler{
		fakeIncomingHandler: fakeIncomingHandler{
			descs: descs,
		},
	}

	session, err := newReceiveSessionWithState(
		ctx, sid, notified, EnvConfig{},
	)
	require.NoError(t, err)

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore:   store,
			IncomingHandler: handler,
			ServerConn:      fakeServerConnRef{},
		},
		actorID:   ActorIDForSession(sid),
		log:       btclog.Disabled,
		sessionID: sid,
		direction: clientdb.OORSessionDirectionIncoming,
		fsm:       session.FSM,
		loaded:    true,
	}

	ax := fakeExec{tx: oorTx{
		store:   &fakeDeliveryStore{},
		actorID: b.actorID,
	}}

	// First turn: materialization runs and drives the in-memory FSM to
	// ReceiveCompleted, but the snapshot upsert fails so the commit rolls
	// back. The behavior must mark itself dirty rather than leave a
	// terminal FSM standing on an uncommitted advance.
	drive := &DriveEventRequest{
		SessionID: sid,
		Event:     &IncomingMetadataResolvedEvent{},
	}
	res := b.Receive(ctx, drive, ax)
	require.True(t, res.IsErr())
	require.ErrorIs(t, res.Err(), errFilterBroken)
	require.Equal(t, 1, handler.calls)
	require.True(t, b.commitFailed)

	// The durable row is still the last-committed ReceiveNotified, not a
	// Completed snapshot.
	stored, err := store.GetSession(ctx, chainHashOf(sid))
	require.NoError(t, err)
	require.False(t, stored.Status.IsTerminal())

	// Redelivery: the reload guard rebuilds the FSM from the
	// ReceiveNotified row, so the same event re-runs materialization and
	// this time the commit lands a terminal Completed snapshot.
	res = b.Receive(ctx, drive, ax)
	require.True(t, res.IsOk(), res.Err())
	require.False(t, b.commitFailed)
	require.Equal(t, 2, handler.calls)

	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &ReceiveCompleted{}, state)
	require.True(t, b.terminalCommitted)

	stored, err = store.GetSession(ctx, chainHashOf(sid))
	require.NoError(t, err)
	require.True(t, stored.Status.IsTerminal())
}

// TestSessionActorStaleDuplicateEventDiscarded verifies a duplicate event
// re-delivered after the FSM advanced past it (the restart-duplicate shape)
// is treated as a benign no-op: the turn commits, the state is unchanged, and
// no side effect runs twice.
func TestSessionActorStaleDuplicateEventDiscarded(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	var spends int
	completer := func(context.Context, []wire.OutPoint) error {
		spends++

		return nil
	}

	pkgStore := &fakePackageStore{}
	b, sessionID, _ := finalizeBehavior(
		t, newFakeRegistryStore(), pkgStore, completer,
	)
	b.cfg.ServerConn = fakeServerConnRef{}

	ax := fakeExec{tx: oorTx{
		store:   &fakeDeliveryStore{},
		actorID: b.actorID,
	}}

	drive := &DriveEventRequest{
		SessionID: sessionID,
		Event:     &FinalizeAcceptedEvent{},
	}

	res := b.Receive(ctx, drive, ax)
	require.True(t, res.IsOk(), res.Err())
	require.Equal(t, 1, spends)
	require.Equal(t, 1, pkgStore.packages)

	// The duplicate finds the FSM in Completed: no spend, no package
	// write, no error.
	res = b.Receive(ctx, drive, ax)
	require.True(t, res.IsOk(), res.Err())

	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &Completed{}, state)
	require.Equal(t, 1, spends)
	require.Equal(t, 1, pkgStore.packages)
}

// failingReservationStore fails every reservation upsert.
type failingReservationStore struct {
	err error
}

func (s *failingReservationStore) UpsertReservation(context.Context,
	wire.OutPoint, int, chainhash.Hash) error {

	return s.err
}

// TestSessionActorReservationWriteErrorPropagates verifies a failed
// spending-reservation write surfaces from recordReservations (and therefore
// fails the admission turn whose commit work stages it), while a nil store
// stays a configured no-op.
func TestSessionActorReservationWriteErrorPropagates(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	inputs := testRetryTransferInputs(t)

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			ReservationStore: &failingReservationStore{
				err: errFilterBroken,
			},
		},
		log:       btclog.Disabled,
		sessionID: oorSessionID(0x72),
	}

	err := b.recordReservations(ctx, inputs)
	require.ErrorIs(t, err, errFilterBroken)

	b.cfg.ReservationStore = nil
	require.NoError(t, b.recordReservations(ctx, inputs))
}

// TestSessionActorTransportTellFailureFailsTurn verifies a failed direct
// transport Tell into the serverconn durable actor aborts the turn so the
// framework redelivers it, instead of committing a snapshot whose implied
// transport was never durably enqueued.
func TestSessionActorTransportTellFailureFailsTurn(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	b, sessionID, _ := finalizeBehavior(
		t, newFakeRegistryStore(), &fakePackageStore{},
		func(context.Context, []wire.OutPoint) error { return nil },
	)
	b.cfg.ServerConn = failingServerConnRef{err: errFilterBroken}

	ax := fakeExec{tx: oorTx{
		store:   &fakeDeliveryStore{},
		actorID: b.actorID,
	}}

	// The resume re-emits the finalize transport; its Tell failure must
	// roll the turn back.
	res := b.Receive(ctx, &ResumeSessionRequest{
		SessionID: sessionID,
	}, ax)
	require.True(t, res.IsErr())
	require.ErrorIs(t, res.Err(), errFilterBroken)
}

// fakeVTXOStore resolves a fixed set of locally-known outpoints and records
// direct status writes; every other VTXOStore method panics via the embedded
// nil interface.
type fakeVTXOStore struct {
	vtxo.VTXOStore

	known   map[wire.OutPoint]*vtxo.Descriptor
	updated []wire.OutPoint
}

func (s *fakeVTXOStore) GetVTXO(_ context.Context, op wire.OutPoint) (
	*vtxo.Descriptor, error) {

	if desc, ok := s.known[op]; ok {
		return desc, nil
	}

	return nil, sql.ErrNoRows
}

func (s *fakeVTXOStore) UpdateVTXOStatus(_ context.Context, op wire.OutPoint,
	_ vtxo.VTXOStatus) error {

	s.updated = append(s.updated, op)

	return nil
}

// TestSessionActorCompleteSpendFilterAndFallback verifies the input-spend
// completion path: non-local outpoints are filtered out, a transient
// completer failure propagates (so the framework retries the turn), and the
// store-fallback path writes spent statuses directly when no completer is
// wired.
func TestSessionActorCompleteSpendFilterAndFallback(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	local := wire.OutPoint{Hash: chainhash.Hash{0x74}, Index: 0}
	foreign := wire.OutPoint{Hash: chainhash.Hash{0x75}, Index: 1}
	store := func() *fakeVTXOStore {
		return &fakeVTXOStore{
			known: map[wire.OutPoint]*vtxo.Descriptor{
				local: {
					Outpoint: local,
				},
			},
		}
	}

	// The completer only sees the locally-known outpoint.
	var completed []wire.OutPoint
	b := &sessionBehavior{
		cfg: SessionActorConfig{
			VTXOStore: store(),
			SpendCompleter: func(_ context.Context,
				ops []wire.OutPoint) error {

				completed = append(completed, ops...)

				return nil
			},
		},
		log: btclog.Disabled,
	}
	err := b.completeSpend(ctx, []wire.OutPoint{local, foreign})
	require.NoError(t, err)
	require.Equal(t, []wire.OutPoint{local}, completed)

	// All-foreign input sets are a no-op.
	completed = nil
	err = b.completeSpend(ctx, []wire.OutPoint{foreign})
	require.NoError(t, err)
	require.Empty(t, completed)

	// A transient completer failure propagates to the caller, failing the
	// turn so the durable framework redelivers it.
	b.cfg.SpendCompleter = func(context.Context, []wire.OutPoint) error {
		return errFilterBroken
	}
	err = b.completeSpend(ctx, []wire.OutPoint{local})
	require.ErrorIs(t, err, errFilterBroken)

	// Without a completer, the spent status is written to the store
	// directly.
	fallback := store()
	b.cfg.SpendCompleter = nil
	b.cfg.VTXOStore = fallback
	err = b.completeSpend(ctx, []wire.OutPoint{local, foreign})
	require.NoError(t, err)
	require.Equal(t, []wire.OutPoint{local}, fallback.updated)
}

// TestSessionActorReleaseInputsBestEffort verifies the pre-point-of-no-return
// input-release path the per-session actor drives when a terminal failure
// emits a ReleaseInputsRequest. Locally-known outpoints route to the
// SpendReleaser while non-local inputs are filtered out, and -- the property
// that closes the C-1 wedge -- a releaser failure is swallowed so driveOutbox
// still commits the turn instead of treating the event as unhandled and
// redelivering the already-terminal session forever.
func TestSessionActorReleaseInputsBestEffort(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sid := oorSessionID(0x76)

	local := wire.OutPoint{Hash: chainhash.Hash{0x76}, Index: 0}
	foreign := wire.OutPoint{Hash: chainhash.Hash{0x77}, Index: 1}
	store := &fakeVTXOStore{
		known: map[wire.OutPoint]*vtxo.Descriptor{
			local: {
				Outpoint: local,
			},
		},
	}

	// releaseSpend filters to the locally-known reserved input; the
	// counterparty's input carries no local reservation to release.
	var released []wire.OutPoint
	b := &sessionBehavior{
		cfg: SessionActorConfig{
			VTXOStore: store,
			SpendReleaser: func(_ context.Context,
				ops []wire.OutPoint) error {

				released = append(released, ops...)

				return nil
			},
		},
		log:       btclog.Disabled,
		sessionID: sid,
	}
	require.NoError(
		t,
		b.releaseSpend(
			ctx, []wire.OutPoint{local, foreign},
		),
	)
	require.Equal(t, []wire.OutPoint{local}, released)

	// The full driveOutbox path is best-effort: a releaser error must NOT
	// fail the turn. Returning the error here would re-drive the terminal
	// transition that emitted the release and wedge the session in a
	// redelivery loop, the exact regression this case guards against.
	b.cfg.SpendReleaser = func(context.Context, []wire.OutPoint) error {
		return errFilterBroken
	}
	release := &ReleaseInputsRequest{
		Outpoints: []wire.OutPoint{
			local,
		},
	}
	require.NoError(t, b.driveOutbox(ctx, []OutboxEvent{release}))
}

// orderRecordingStore appends a marker when the snapshot upsert runs so tests
// can assert commit-phase ordering.
type orderRecordingStore struct {
	*fakeRegistryStore

	order *[]string
}

func (s *orderRecordingStore) UpsertSession(ctx context.Context,
	record clientdb.OORSessionRegistryRecord) error {

	*s.order = append(*s.order, "snapshot")

	return s.fakeRegistryStore.UpsertSession(ctx, record)
}

// orderPackageStore appends a marker when the finalized package persists.
type orderPackageStore struct {
	order *[]string
}

func (s *orderPackageStore) UpsertPackage(_ context.Context, _ PackageDirection,
	_ chainhash.Hash, _ *psbt.Packet, _ []*psbt.Packet) error {

	*s.order = append(*s.order, "package")

	return nil
}

func (s *orderPackageStore) UpsertBinding(_ context.Context, _ wire.OutPoint,
	_ chainhash.Hash, _ uint32, _ PackageLinkKind) error {

	return nil
}

// leaseLostExec models a lost lease: the commit fails without running the
// closure, so nothing the turn staged may be observable.
type leaseLostExec struct {
	fakeExec
}

func (e leaseLostExec) Commit(context.Context,
	func(context.Context, oorTx) error) error {

	return actor.ErrLeaseLost
}

// TestSessionActorCommitAckOrdering verifies the turn completes the input
// spend (inline in dispatch, no writer held) before the commit phase persists
// the finalized package and the snapshot upsert (the materialize closure
// advances the FSM, so the snapshot must observe the final state), and that a
// lost lease rolls the whole turn back with nothing observable.
func TestSessionActorCommitAckOrdering(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	var order []string
	store := &orderRecordingStore{
		fakeRegistryStore: newFakeRegistryStore(),
		order:             &order,
	}
	pkgStore := &orderPackageStore{order: &order}
	completer := func(context.Context, []wire.OutPoint) error {
		order = append(order, "spend")

		return nil
	}

	b, sessionID, _ := finalizeBehavior(t, store, pkgStore, completer)
	b.cfg.ServerConn = fakeServerConnRef{}

	ax := fakeExec{tx: oorTx{
		store:   &fakeDeliveryStore{},
		actorID: b.actorID,
	}}

	res := b.Receive(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event:     &FinalizeAcceptedEvent{},
	}, ax)
	require.True(t, res.IsOk(), res.Err())
	require.Equal(t, []string{"spend", "package", "snapshot"}, order)

	// A lost lease surfaces as the commit error and leaves no terminal
	// mark behind.
	b2, sid2, _ := finalizeBehavior(
		t, newFakeRegistryStore(), &fakePackageStore{},
		func(context.Context, []wire.OutPoint) error { return nil },
	)
	b2.cfg.ServerConn = fakeServerConnRef{}

	res = b2.Receive(ctx, &DriveEventRequest{
		SessionID: sid2,
		Event:     &FinalizeAcceptedEvent{},
	}, leaseLostExec{})
	require.True(t, res.IsErr())
	require.ErrorIs(t, res.Err(), actor.ErrLeaseLost)
	require.False(t, b2.terminalCommitted)
}

// TestSessionActorIncomingRestoreFromRecord verifies an incoming session is
// rebuilt from its persisted registry record: the snapshot decodes through
// the configured receive limits and the restored FSM resumes in the exact
// persisted state, attempts counter included.
func TestSessionActorIncomingRestoreFromRecord(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// The incoming snapshot decode validates the session id against the
	// embedded Ark txid, so the fixture id must be derived from the PSBT.
	ark, checkpoints := testOutboxPSBTPair(t)
	sid, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	record, err := incomingRegistryRecord(sid, &ReceiveNotified{
		SessionID:            sid,
		ArkPSBT:              ark,
		FinalCheckpointPSBTs: checkpoints,
		MetadataAttempts:     2,
	})
	require.NoError(t, err)

	store := newFakeRegistryStore()
	require.NoError(t, store.UpsertSession(ctx, record))

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore: store,
			Limits:        DefaultReceiveLimits(),
		},
		log:       btclog.Disabled,
		sessionID: sid,
		direction: clientdb.OORSessionDirectionIncoming,
	}

	require.NoError(t, b.restore(ctx))
	require.True(t, b.loaded)
	require.Equal(t, clientdb.OORSessionDirectionIncoming, b.direction)

	state, err := b.fsm.CurrentState()
	require.NoError(t, err)

	notified, ok := state.(*ReceiveNotified)
	require.True(t, ok, "expected ReceiveNotified, got %T", state)
	require.Equal(t, uint32(2), notified.MetadataAttempts)
}

// TestOutboxForStateTable verifies every outgoing state maps to the transport
// it implies on resume, so a restored session re-emits exactly the message
// the operator is waiting on.
func TestOutboxForStateTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		state State
		want  OutboxEvent

		// wantSchedule asserts the state also arms a
		// ScheduleRetryRequest re-drive timer alongside its primary
		// transport. The outgoing wait states that have no operator
		// failure path arm one so a dead-lettered transport re-drives
		// instead of wedging the session until restart.
		wantSchedule bool
	}{
		{
			name:  "ark signatures",
			state: &AwaitingArkSignatures{},
			want:  &RequestArkSignatures{},
		},
		{
			name:         "submit accepted",
			state:        &AwaitingSubmitAccepted{},
			want:         &SendSubmitPackageRequest{},
			wantSchedule: true,
		},
		{
			name:  "checkpoint signatures",
			state: &AwaitingCheckpointSignatures{},
			want:  &RequestCheckpointSignatures{},
		},
		{
			name:         "finalize accepted",
			state:        &AwaitingFinalizeAccepted{},
			want:         &SendFinalizePackageRequest{},
			wantSchedule: true,
		},
		{
			name:         "local vtxo update",
			state:        &AwaitingLocalVTXOUpdate{},
			want:         &MarkInputsSpentRequest{},
			wantSchedule: true,
		},
		{
			name:  "completed",
			state: &Completed{},
		},
		{
			name:  "failed",
			state: &Failed{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			outbox, err := OutboxForState(tc.state)
			require.NoError(t, err)

			if tc.want == nil {
				require.Empty(t, outbox)

				return
			}

			require.IsType(t, tc.want, outbox[0])

			if tc.wantSchedule {
				require.Len(t, outbox, 2)
				require.IsType(
					t, &ScheduleRetryRequest{}, outbox[1],
				)

				return
			}

			require.Len(t, outbox, 1)
		})
	}

	_, err := OutboxForState(nil)
	require.Error(t, err)
}

// TestOutboxForStateLocalVTXOUpdateArmsRedriveTimer pins that a session resumed
// in AwaitingLocalVTXOUpdate re-emits BOTH the input-spend completion request
// and a re-drive timer. Unlike the operator-driven waits, nothing answers this
// state except completeSpend succeeding, so a resume whose spend fails would
// wedge until the next daemon restart without the timer. The timer re-drives
// the idempotent spend on the bounded outgoing-transport cadence.
func TestOutboxForStateLocalVTXOUpdateArmsRedriveTimer(t *testing.T) {
	t.Parallel()

	inputs := testRetryTransferInputs(t)
	outbox, err := OutboxForState(&AwaitingLocalVTXOUpdate{
		TransferInputs: inputs,
	})
	require.NoError(t, err)

	// The outbox carries the spend request followed by the re-drive timer.
	require.Len(t, outbox, 2)

	spend, ok := outbox[0].(*MarkInputsSpentRequest)
	require.True(
		t, ok, "expected MarkInputsSpentRequest, got %T", outbox[0],
	)
	require.ElementsMatch(t, InputOutpoints(inputs), spend.Outpoints)

	retry, ok := outbox[1].(*ScheduleRetryRequest)
	require.True(t, ok, "expected ScheduleRetryRequest, got %T", outbox[1])
	require.Equal(t, outgoingTransportRedriveInterval, retry.After)
	require.Equal(t, localVTXOUpdateRedriveReason, retry.Reason)
}

// TestOutboxForIncomingStateTable verifies every incoming state maps to the
// transport it implies on resume.
func TestOutboxForIncomingStateTable(t *testing.T) {
	t.Parallel()

	ark, checkpoints := testOutboxPSBTPair(t)
	sid := oorSessionID(0x7a)

	cases := []struct {
		name  string
		state SessionState
		want  OutboxEvent

		// wantSchedule asserts the state also arms a
		// ScheduleRetryRequest give-up timer alongside its primary
		// query.
		wantSchedule bool
	}{
		{
			name: "resolving",
			state: &ReceiveResolving{
				SessionID: sid,
				RecipientPkScript: []byte{
					0x51,
				},
			},
			want:         &QueryIncomingTransferRequest{},
			wantSchedule: true,
		},
		{
			name: "notified",
			state: &ReceiveNotified{
				SessionID:            sid,
				ArkPSBT:              ark,
				FinalCheckpointPSBTs: checkpoints,
			},
			want:         &QueryIncomingMetadataRequest{},
			wantSchedule: true,
		},
		{
			name: "awaiting ack",
			state: &ReceiveAwaitingAck{
				SessionID: sid,
			},
			want: &SendIncomingAckRequest{},
		},
		{
			name:  "completed",
			state: &ReceiveCompleted{},
		},
		{
			name:  "failed",
			state: &Failed{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			outbox, err := OutboxForIncomingState(tc.state)
			require.NoError(t, err)

			if tc.want == nil {
				require.Empty(t, outbox)

				return
			}

			require.IsType(t, tc.want, outbox[0])

			if tc.wantSchedule {
				require.Len(t, outbox, 2)
				require.IsType(
					t, &ScheduleRetryRequest{}, outbox[1],
				)

				return
			}

			require.Len(t, outbox, 1)
		})
	}

	_, err := OutboxForIncomingState(nil)
	require.Error(t, err)
}

// TestSessionActorBuildTransportMessage verifies the transport adapter: the
// two incoming indexer queries map to their dedicated durable unary requests
// (with the event cursor and recipient scripts threaded through), every
// ServerMessage transport wraps into a client-event request, and a
// non-transport event is rejected.
func TestSessionActorBuildTransportMessage(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sid := oorSessionID(0x7b)

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			Limits: DefaultReceiveLimits(),
		},
		log:       btclog.Disabled,
		sessionID: sid,
	}

	// The resolve query lists recipient events strictly after the cursor
	// preceding the hint's event id.
	msg, err := b.buildTransportMessage(ctx, &QueryIncomingTransferRequest{
		SessionID:         sid,
		RecipientPkScript: []byte{0x51, 0x20},
		RecipientEventID:  5,
	})
	require.NoError(t, err)

	listEvents, ok :=
		msg.(*serverconn.SendListOORRecipientEventsByScriptRequest)
	require.True(t, ok, "expected list-events request, got %T", msg)
	require.Equal(t, uint64(4), listEvents.AfterEventID)
	require.Equal(t, []byte{0x51, 0x20}, listEvents.PkScript)

	// A zero event id clamps the cursor at zero instead of underflowing.
	msg, err = b.buildTransportMessage(ctx, &QueryIncomingTransferRequest{
		SessionID:         sid,
		RecipientPkScript: []byte{0x51},
	})
	require.NoError(t, err)

	listEvents, ok =
		msg.(*serverconn.SendListOORRecipientEventsByScriptRequest)
	require.True(t, ok)
	require.Zero(t, listEvents.AfterEventID)

	// The metadata query carries one pkScript per recipient and the
	// configured match cap.
	msg, err = b.buildTransportMessage(ctx, &QueryIncomingMetadataRequest{
		SessionID: sid,
		Recipients: []ArkRecipientOutput{
			{OutputIndex: 0, PkScript: []byte{0x51}},
			{OutputIndex: 1, PkScript: []byte{0x52}},
		},
	})
	require.NoError(t, err)

	listVTXOs, ok := msg.(*serverconn.SendListVTXOsByScriptsRequest)
	require.True(t, ok, "expected list-vtxos request, got %T", msg)
	require.Equal(t, [][]byte{{0x51}, {0x52}}, listVTXOs.PkScripts)
	require.Equal(t, DefaultReceiveLimits().MaxVTXOMatches, listVTXOs.Limit)

	// A metadata query with no wallet-owned recipients is rejected.
	_, err = b.buildTransportMessage(ctx, &QueryIncomingMetadataRequest{
		SessionID: sid,
	})
	require.ErrorContains(t, err, "no wallet-owned recipients")

	// A non-transport outbox event is rejected.
	_, err = b.buildTransportMessage(ctx, &ScheduleRetryRequest{})
	require.ErrorContains(t, err, "does not implement ServerMessage")
}

// TestSessionActorMetadataQueryEmptyRecipientsFailsTerminally verifies that a
// deterministic transport-build error (operator-supplied metadata query whose
// recipient set contains nothing this wallet owns) fails the session terminally
// instead of returning an error that would roll the turn back and make the
// durable mailbox redeliver the doomed turn forever. The turn must succeed (so
// the message acks) and the FSM must land in Failed.
func TestSessionActorMetadataQueryEmptyRecipientsFailsTerminally(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sid := oorSessionID(0x51)

	session, err := newReceiveSessionWithState(
		ctx, sid, &ReceiveNotified{
			SessionID: sid,
		},
		EnvConfig{},
	)
	require.NoError(t, err)

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			// owned:false => the filter returns no owned
			// recipients, so buildTransportMessage reports the
			// deterministic terminal error.
			IncomingHandler: &fakeRecipientFilter{
				owned: false,
			},
			ServerConn: fakeServerConnRef{},
		},
		actorID:   ActorIDForSession(sid),
		log:       btclog.Disabled,
		sessionID: sid,
		direction: clientdb.OORSessionDirectionIncoming,
		fsm:       session.FSM,
		loaded:    true,
	}
	t.Cleanup(b.stopFSM)

	query := &QueryIncomingMetadataRequest{
		SessionID: sid,
		Recipients: []ArkRecipientOutput{
			{
				OutputIndex: 0,
				PkScript: []byte{
					0x51,
					0x20,
				},
			},
		},
	}

	// The deterministic error is converted into a terminal FailEvent, so
	// driveOutbox returns no error: the turn commits and acks.
	require.NoError(t, b.driveOutbox(ctx, []OutboxEvent{query}))

	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &Failed{}, state)
}

// TestSessionActorMetadataQueryTransientErrorRetries verifies that a TRANSIENT
// transport-build error (the recipient filter itself erroring, e.g. a busy
// store) is propagated rather than converted to a terminal failure, so the
// framework redelivers the turn and the session stays alive.
func TestSessionActorMetadataQueryTransientErrorRetries(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sid := oorSessionID(0x52)

	session, err := newReceiveSessionWithState(
		ctx, sid, &ReceiveNotified{
			SessionID: sid,
		},
		EnvConfig{},
	)
	require.NoError(t, err)

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			IncomingHandler: &fakeRecipientFilter{
				err: errFilterBroken,
			},
			ServerConn: fakeServerConnRef{},
		},
		actorID:   ActorIDForSession(sid),
		log:       btclog.Disabled,
		sessionID: sid,
		direction: clientdb.OORSessionDirectionIncoming,
		fsm:       session.FSM,
		loaded:    true,
	}
	t.Cleanup(b.stopFSM)

	query := &QueryIncomingMetadataRequest{
		SessionID: sid,
		Recipients: []ArkRecipientOutput{
			{
				OutputIndex: 0,
				PkScript: []byte{
					0x51,
					0x20,
				},
			},
		},
	}

	// The transient error propagates (turn rolls back, redelivers); the
	// session is NOT failed.
	err = b.driveOutbox(ctx, []OutboxEvent{query})
	require.ErrorIs(t, err, errFilterBroken)

	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &ReceiveNotified{}, state)
}
