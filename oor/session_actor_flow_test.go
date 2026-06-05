package oor

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	clientdb "github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

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
	var observed []*vtxo.Descriptor
	var inCommit bool
	sink := &recordingLedgerSink{inCommit: &inCommit}
	delivery := &fakeDeliveryStore{}

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore: newFakeRegistryStore(),
			IncomingHandler: &fakeIncomingHandler{
				descs: descs,
			},
			VTXOManager: manager,
			IncomingVTXOObserver: func(_ context.Context,
				d []*vtxo.Descriptor) error {

				observed = append(observed, d...)

				return nil
			},
			LedgerSink: fn.Some[ledger.Sink](sink),
			ServerConn: fakeServerConnRef{},
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
	require.Len(t, delivery.enqueued, 1)

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

	// The post-commit best-effort notifications fired for the VTXO manager
	// and the fraud observer with the full descriptor set.
	mgrMsgs := manager.recorded()
	require.Len(t, mgrMsgs, 1)
	notify, ok := mgrMsgs[0].(*vtxo.VTXOsMaterializedNotification)
	require.True(t, ok)
	require.Len(t, notify.VTXOs, len(descs))
	require.Len(t, observed, len(descs))
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

// failingDeliveryStore fails every durable outbox enqueue.
type failingDeliveryStore struct {
	actor.DeliveryStore

	err error
}

func (s *failingDeliveryStore) EnqueueOutbox(context.Context,
	actor.OutboxParams) error {

	return s.err
}

// TestSessionActorTransportEnqueueFailureFailsTurn verifies a failed durable
// transport enqueue aborts the turn so the framework redelivers it, instead
// of committing a snapshot whose implied transport was never queued.
func TestSessionActorTransportEnqueueFailureFailsTurn(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	b, sessionID, _ := finalizeBehavior(
		t, newFakeRegistryStore(), &fakePackageStore{},
		func(context.Context, []wire.OutPoint) error { return nil },
	)
	b.cfg.ServerConn = fakeServerConnRef{}

	ax := fakeExec{tx: oorTx{
		store: &failingDeliveryStore{
			err: errFilterBroken,
		},
		actorID: b.actorID,
	}}

	// The resume re-emits the finalize transport; its enqueue failure must
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

// TestSessionActorCommitAckOrdering verifies the commit phase runs the staged
// work strictly before the snapshot upsert (the materialize closure advances
// the FSM, so the snapshot must observe the final state), and that a lost
// lease rolls the whole turn back with nothing observable.
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
	}{
		{
			name:  "ark signatures",
			state: &AwaitingArkSignatures{},
			want:  &RequestArkSignatures{},
		},
		{
			name:  "submit accepted",
			state: &AwaitingSubmitAccepted{},
			want:  &SendSubmitPackageRequest{},
		},
		{
			name:  "checkpoint signatures",
			state: &AwaitingCheckpointSignatures{},
			want:  &RequestCheckpointSignatures{},
		},
		{
			name:  "finalize accepted",
			state: &AwaitingFinalizeAccepted{},
			want:  &SendFinalizePackageRequest{},
		},
		{
			name:  "local vtxo update",
			state: &AwaitingLocalVTXOUpdate{},
			want:  &MarkInputsSpentRequest{},
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

			require.Len(t, outbox, 1)
			require.IsType(t, tc.want, outbox[0])
		})
	}

	_, err := OutboxForState(nil)
	require.Error(t, err)
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
	}{
		{
			name: "resolving",
			state: &ReceiveResolving{
				SessionID: sid,
				RecipientPkScript: []byte{
					0x51,
				},
			},
			want: &QueryIncomingTransferRequest{},
		},
		{
			name: "notified",
			state: &ReceiveNotified{
				SessionID:            sid,
				ArkPSBT:              ark,
				FinalCheckpointPSBTs: checkpoints,
			},
			want: &QueryIncomingMetadataRequest{},
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

			require.Len(t, outbox, 1)
			require.IsType(t, tc.want, outbox[0])
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
