package oor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/timeout"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// fakeRegistryStore is an in-memory SessionRegistryStore for unit tests. The
// mutex makes it safe for end-to-end tests where real child actors write
// snapshots concurrently with registry-goroutine reads.
type fakeRegistryStore struct {
	mu   sync.Mutex
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

	s.mu.Lock()
	defer s.mu.Unlock()

	s.rows[record.SessionID] = record

	return nil
}

func (s *fakeRegistryStore) GetSession(_ context.Context,
	sessionID chainhash.Hash) (*clientdb.OORSessionRegistryRecord, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.rows[sessionID]
	if !ok {
		return nil, clientdb.ErrOORSessionNotFound
	}

	return &record, nil
}

func (s *fakeRegistryStore) LookupActiveSessionByIdempotencyKey(
	_ context.Context, key string) (*clientdb.OORSessionRegistryRecord,
	error) {

	s.mu.Lock()
	defer s.mu.Unlock()

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

	s.mu.Lock()
	defer s.mu.Unlock()

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

	// The input spend runs inline in dispatch (no OOR writer held), so it
	// is already complete before any commit work runs, and it is NOT staged
	// into commitWork (a cross-actor write under a held OOR writer would
	// deadlock).
	require.ElementsMatch(t, wantOutpoints, spent)

	// Run the staged commit work against a dummy tx store; only the package
	// persistence is deferred to the commit transaction.
	for _, work := range b.commitWork {
		require.NoError(t, work(ctx, oorTx{}))
	}

	require.Equal(t, 1, pkgStore.packages)

	// The snapshot for the completed session must report terminal status.
	record, err := b.snapshotRecord()
	require.NoError(t, err)
	require.Equal(t, clientdb.OORSessionStatusCompleted, record.Status)
	require.Equal(t, string(OutgoingPhaseCompleted), record.Phase)
}

// TestSessionActorSpendFailsTurnBeforeCommit verifies the input-spend
// completion runs inline in dispatch (no OOR writer held): a SpendCompleter
// failure fails the dispatch turn outright, so the FSM never advances to
// Completed and nothing is staged for the commit transaction. This pins that
// the cross-actor spend Ask is never awaited inside the held commit writer tx,
// where a second writer would deadlock under the single SQLite/Postgres lock.
func TestSessionActorSpendFailsTurnBeforeCommit(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	completer := func(context.Context, []wire.OutPoint) error {
		return errFilterBroken
	}

	registry := newFakeRegistryStore()
	pkgStore := &fakePackageStore{}
	b, sessionID, _ := finalizeBehavior(t, registry, pkgStore, completer)

	b.pendingTransport = b.pendingTransport[:0]
	b.commitWork = b.commitWork[:0]
	b.postCommit = b.postCommit[:0]

	res := b.dispatch(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event:     &FinalizeAcceptedEvent{},
	})
	require.True(t, res.IsErr())
	require.ErrorIs(t, res.Err(), errFilterBroken)

	// The FSM is still parked at AwaitingLocalVTXOUpdate: a failed spend
	// must not advance it to Completed, so a boot-time resume re-emits
	// MarkInputsSpentRequest and the manager's idempotent replay completes
	// the spend.
	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &AwaitingLocalVTXOUpdate{}, state)

	// Nothing was staged for the commit transaction: the failure short-
	// circuited dispatch before the package work was appended.
	require.Empty(t, b.commitWork)
	require.Equal(t, 0, pkgStore.packages)
}

// uniqueViolationStore wraps a fakeRegistryStore and fails the first
// UpsertSession with a unique-constraint violation, modeling the Postgres
// idempotency-key race where the loser's snapshot collides on the partial
// UNIQUE index. Subsequent upserts succeed.
type uniqueViolationStore struct {
	*fakeRegistryStore

	violationsLeft int
}

func (s *uniqueViolationStore) UpsertSession(ctx context.Context,
	record clientdb.OORSessionRegistryRecord) error {

	if s.violationsLeft > 0 {
		s.violationsLeft--

		return &clientdb.ErrSQLUniqueConstraintViolation{
			DBError: fmt.Errorf("duplicate idempotency key"),
		}
	}

	return s.fakeRegistryStore.UpsertSession(ctx, record)
}

// TestSessionActorDedupRaceRedeliversThenDedups pins the Postgres idempotency-
// key race fix: the loser's resolveKeyDedup SELECT misses the winner, its
// snapshot upsert collides on the partial UNIQUE index, and commitAck surfaces
// that as a unique-constraint error so the turn rolls back and redelivers
// (rather than silently consuming as a clean dedup). On the redelivered turn
// the winner's row is visible, so resolveKeyDedup consumes the turn cleanly as
// Existing.
func TestSessionActorDedupRaceRedeliversThenDedups(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	const key = "shared-idem-key"

	// Build a keyed outgoing behavior parked at AwaitingFinalizeAccepted.
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
			IdempotencyKey:       key,
		},
	)
	require.NoError(t, err)

	store := &uniqueViolationStore{
		fakeRegistryStore: newFakeRegistryStore(),
		violationsLeft:    1,
	}

	rec := &registrySpawnRecorder{}
	registryRef := &recordingTellRef{id: "oor-registry", rec: rec}

	build := func() *sessionBehavior {
		s, serr := NewSessionFromSnapshot(ctx, snapshot)
		require.NoError(t, serr)

		return &sessionBehavior{
			cfg: SessionActorConfig{
				RegistryStore: store,
				PackageStore:  &fakePackageStore{},
				SpendCompleter: func(context.Context,
					[]wire.OutPoint) error {

					return nil
				},
				Registry: registryRef,
			},
			actorID:   ActorIDForSession(sessionID),
			log:       btclog.Disabled,
			sessionID: sessionID,
			direction: clientdb.OORSessionDirectionOutgoing,
			fsm:       s.FSM,
			loaded:    true,
		}
	}

	ax := fakeExec{tx: oorTx{
		store:   &fakeDeliveryStore{},
		actorID: ActorIDForSession(sessionID),
	}}

	// First turn: the upsert collides; the turn must error so the durable
	// framework redelivers it. It must NOT silently report a clean dedup.
	loser := build()
	res := loser.Receive(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event:     &FinalizeAcceptedEvent{},
	}, ax)
	require.True(t, res.IsErr())
	require.Nil(t, loser.dedupKeyWinner)

	// The winner's row lands (a different session id, same key). The
	// redelivered loser's resolveKeyDedup now sees it and consumes cleanly.
	winnerID := oorSessionID(0xee)
	winnerRow := clientdb.OORSessionRegistryRecord{
		SessionID: chainHashOf(winnerID),
		Direction: clientdb.
			OORSessionDirectionOutgoing,
		IdempotencyKey: key,
		Status:         clientdb.OORSessionStatusPending,
	}
	require.NoError(t, store.UpsertSession(ctx, winnerRow))

	redelivered := build()
	res = redelivered.Receive(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event:     &FinalizeAcceptedEvent{},
	}, ax)
	require.True(t, res.IsOk(), res.Err())

	resp, ok := res.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)
	require.True(t, resp.Existing)
	require.Equal(t, winnerID, resp.SessionID)

	// The dedup loser wrote no durable row, so it is an orphaned child. The
	// dedup turn must notify the registry so it gets reaped instead of
	// leaking a live actor goroutine + mailbox + receptionist key for a
	// session id with no backing row.
	require.Eventually(t, func() bool {
		msgs := rec.recorded()
		if len(msgs) != 1 {
			return false
		}

		notify, ok := msgs[0].(*SessionTerminalNotification)

		return ok && notify.SessionID == sessionID
	}, time.Second, 10*time.Millisecond)
}

// TestSessionActorIncomingResolve verifies admitting an incoming transfer hint
// installs a ReceiveResolving session and collects the phase-1 indexer query as
// cross-actor transport.
func TestSessionActorIncomingResolve(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore: newFakeRegistryStore(),
		},
		log: btclog.Disabled,
	}

	sid := oorSessionID(0x60)
	res := b.handleResolveIncomingTransfer(
		ctx, &ResolveIncomingTransferRequest{
			SessionID:         sid,
			RecipientPkScript: []byte{0x51, 0x20, 0xaa, 0xbb},
			RecipientEventID:  7,
		},
	)
	require.True(t, res.IsOk(), res.Err())

	require.True(t, b.loaded)
	require.Equal(t, clientdb.OORSessionDirectionIncoming, b.direction)

	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &ReceiveResolving{}, state)

	require.Len(t, b.pendingTransport, 1)
	query := b.pendingTransport[0]
	_, ok := query.(*serverconn.SendListOORRecipientEventsByScriptRequest)
	require.True(t, ok)
}

// TestSessionActorResolveResumeDrivesGiveUp verifies a resolving session's
// give-up timer expiry, delivered as a ResumeSessionRequest, drives the FSM
// toward the terminal give-up rather than merely re-emitting the query. One
// resume below the bound re-queries and re-arms; a resume at the bound fails
// the session so it becomes reap-eligible and frees its concurrency slot.
func TestSessionActorResolveResumeDrivesGiveUp(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	sid := oorSessionID(0x61)
	build := func(attempts uint32) *sessionBehavior {
		session, err := newReceiveSessionWithState(
			ctx, sid, &ReceiveResolving{
				SessionID:         sid,
				RecipientPkScript: []byte{0x51, 0x20, 0xaa},
				ResolveAttempts:   attempts,
			},
		)
		require.NoError(t, err)

		return &sessionBehavior{
			cfg: SessionActorConfig{
				RegistryStore: newFakeRegistryStore(),
			},
			log:       btclog.Disabled,
			sessionID: sid,
			direction: clientdb.OORSessionDirectionIncoming,
			fsm:       session.FSM,
			loaded:    true,
		}
	}

	// Below the bound: a timer-driven resume re-queries and re-arms,
	// advancing the persisted attempt counter, and the session stays
	// resolving.
	below := build(0)
	res := below.dispatch(ctx, &ResumeSessionRequest{
		SessionID:      sid,
		FromRetryTimer: true,
	})
	require.True(t, res.IsOk(), res.Err())

	state, err := below.fsm.CurrentState()
	require.NoError(t, err)
	resolving, ok := state.(*ReceiveResolving)
	require.True(t, ok)
	require.Equal(t, uint32(1), resolving.ResolveAttempts)

	// At the bound: the timer-driven resume fails the session terminally so
	// its slot can be reaped.
	atCap := build(maxResolveRetries)
	res = atCap.dispatch(ctx, &ResumeSessionRequest{
		SessionID:      sid,
		FromRetryTimer: true,
	})
	require.True(t, res.IsOk(), res.Err())

	state, err = atCap.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &Failed{}, state)
}

// TestSessionActorResolveBootResumeNoAmplify verifies that a boot restore
// (ResumeSessionRequest with FromRetryTimer false) of a resolving session does
// NOT advance the persisted ResolveAttempts counter. Only a timer expiry may
// drive the give-up, so N restarts during one backoff window cannot burn N
// attempts and fail the session before its time-based budget elapses.
func TestSessionActorResolveBootResumeNoAmplify(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	sid := oorSessionID(0x66)
	session, err := newReceiveSessionWithState(
		ctx, sid, &ReceiveResolving{
			SessionID:         sid,
			RecipientPkScript: []byte{0x51, 0x20, 0xaa},
			ResolveAttempts:   5,
		},
	)
	require.NoError(t, err)

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore: newFakeRegistryStore(),
		},
		log:       btclog.Disabled,
		sessionID: sid,
		direction: clientdb.OORSessionDirectionIncoming,
		fsm:       session.FSM,
		loaded:    true,
	}

	// Many boot resumes must not advance the attempt counter past its
	// persisted value: the session stays resolving with ResolveAttempts
	// unchanged regardless of restart count.
	for range maxResolveRetries + 5 {
		res := b.dispatch(ctx, &ResumeSessionRequest{SessionID: sid})
		require.True(t, res.IsOk(), res.Err())
	}

	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	resolving, ok := state.(*ReceiveResolving)
	require.True(t, ok)
	require.Equal(t, uint32(5), resolving.ResolveAttempts)
}

// TestSessionActorNotifiedResumeDrivesGiveUp verifies a notified session's
// metadata give-up timer expiry, delivered as a timer-driven
// ResumeSessionRequest, drives the FSM toward the terminal give-up rather than
// re-querying the operator forever. One resume below the bound re-queries and
// re-arms while advancing the persisted attempt counter; a resume at the bound
// fails the session so it becomes reap-eligible and frees its concurrency slot.
func TestSessionActorNotifiedResumeDrivesGiveUp(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	ark, checkpoints := testOutboxPSBTPair(t)
	sid := oorSessionID(0x67)
	build := func(attempts uint32) *sessionBehavior {
		session, err := newReceiveSessionWithState(
			ctx, sid, &ReceiveNotified{
				SessionID:            sid,
				ArkPSBT:              ark,
				FinalCheckpointPSBTs: checkpoints,
				MetadataAttempts:     attempts,
			},
		)
		require.NoError(t, err)

		return &sessionBehavior{
			cfg: SessionActorConfig{
				RegistryStore: newFakeRegistryStore(),
			},
			log:       btclog.Disabled,
			sessionID: sid,
			direction: clientdb.OORSessionDirectionIncoming,
			fsm:       session.FSM,
			loaded:    true,
		}
	}

	// Below the bound: a timer-driven resume re-queries and re-arms,
	// advancing the persisted attempt counter, and the session stays
	// notified.
	below := build(0)
	res := below.dispatch(ctx, &ResumeSessionRequest{
		SessionID:      sid,
		FromRetryTimer: true,
	})
	require.True(t, res.IsOk(), res.Err())

	state, err := below.fsm.CurrentState()
	require.NoError(t, err)
	notified, ok := state.(*ReceiveNotified)
	require.True(t, ok)
	require.Equal(t, uint32(1), notified.MetadataAttempts)

	// The metadata query must be re-collected on a timer-driven resume.
	require.Len(t, below.pendingTransport, 1)
	query := below.pendingTransport[0]
	_, ok = query.(*serverconn.SendListVTXOsByScriptsRequest)
	require.True(t, ok)

	// At the bound: the timer-driven resume fails the session terminally so
	// its slot can be reaped.
	atCap := build(maxMetadataRetries)
	res = atCap.dispatch(ctx, &ResumeSessionRequest{
		SessionID:      sid,
		FromRetryTimer: true,
	})
	require.True(t, res.IsOk(), res.Err())

	state, err = atCap.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &Failed{}, state)
}

// TestSessionActorNotifiedBootResumeNoAmplify verifies that a boot restore
// (ResumeSessionRequest with FromRetryTimer false) of a notified session does
// NOT advance the persisted MetadataAttempts counter and does not re-fire the
// query. Only a timer expiry may drive the give-up, so restarts cannot burn the
// give-up budget faster than the time-based schedule.
func TestSessionActorNotifiedBootResumeNoAmplify(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	ark, checkpoints := testOutboxPSBTPair(t)
	sid := oorSessionID(0x68)
	session, err := newReceiveSessionWithState(
		ctx, sid, &ReceiveNotified{
			SessionID:            sid,
			ArkPSBT:              ark,
			FinalCheckpointPSBTs: checkpoints,
			MetadataAttempts:     4,
		},
	)
	require.NoError(t, err)

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore: newFakeRegistryStore(),
		},
		log:       btclog.Disabled,
		sessionID: sid,
		direction: clientdb.OORSessionDirectionIncoming,
		fsm:       session.FSM,
		loaded:    true,
	}

	for range maxMetadataRetries + 5 {
		res := b.dispatch(ctx, &ResumeSessionRequest{SessionID: sid})
		require.True(t, res.IsOk(), res.Err())
		require.Empty(t, b.pendingTransport)
	}

	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	notified, ok := state.(*ReceiveNotified)
	require.True(t, ok)
	require.Equal(t, uint32(4), notified.MetadataAttempts)
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

// TestSessionActorOutgoingRedriveTimerReDrives pins the dead-letter wedge fix
// for the outgoing flow: a session parked in AwaitingFinalizeAccepted whose
// re-drive timer fires (a ResumeSessionRequest with FromRetryTimer set)
// re-drives the idempotent finalize transport AND re-arms its re-drive timer
// within the same committed turn. Without the timer, a finalize whose
// cross-actor delivery dead-letters would pin the session until a daemon
// restart; the re-driving loop is what lets a wedged in-flight send self-heal
// while the daemon stays up. The outgoing transport never gives up: a peer that
// is merely offline keeps getting re-sent to until it answers.
func TestSessionActorOutgoingRedriveTimerReDrives(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	registry := newFakeRegistryStore()
	b, sessionID, _ := finalizeBehavior(
		t, registry, &fakePackageStore{}, nil,
	)

	// Wire the timeout actor and serverconn so both the re-drive timer and
	// the transport are collected, mirroring the production wiring.
	b.cfg.TimeoutActor = &recordingTimeoutRef{}
	b.cfg.CallbackRef = fakeExpiredRef{}
	b.cfg.ServerConn = fakeServerConnRef{}

	// Drive a timer-fired resume. dispatch collects the outbox without
	// committing, so we can assert the transport and the re-armed re-drive
	// timer were both queued for the turn.
	res := b.dispatch(ctx, &ResumeSessionRequest{
		SessionID:      sessionID,
		FromRetryTimer: true,
	})
	require.True(t, res.IsOk(), res.Err())

	// The finalize transport is re-collected for cross-actor delivery.
	require.Len(t, b.pendingTransport, 1)
	wrapped, ok :=
		b.pendingTransport[0].(*serverconn.SendClientEventRequest)
	require.True(t, ok)
	require.IsType(t, &SendFinalizePackageRequest{}, wrapped.Message)

	// The re-drive timer is re-queued so a still-dead-lettering transport
	// keeps re-driving instead of wedging the session.
	require.Len(t, b.pendingRetries, 1)
	require.Equal(t, finalizeRedriveReason, b.pendingRetries[0].Reason)

	// The FSM stays parked in the awaiting state: the timer re-emits
	// transport, it does not advance the session.
	state, err := b.fsm.CurrentState()
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
// the incoming flow. A boot resume (FromRetryTimer false) of a session restored
// in ReceiveResolving must NOT re-fire the phase-1 query immediately: it only
// re-arms the give-up timer from the persisted attempt count, so restarts do
// not re-spin the operator mailbox. A subsequent timer-driven resume re-emits
// the phase-1 query.
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

	// Drop the admission turn's collected transport, then boot-resume: no
	// query may be re-collected, and the session stays resolving.
	b.pendingTransport = b.pendingTransport[:0]
	b.commitWork = b.commitWork[:0]
	b.postCommit = b.postCommit[:0]

	res = b.dispatch(ctx, &ResumeSessionRequest{SessionID: sid})
	require.True(t, res.IsOk(), res.Err())

	require.Empty(t, b.pendingTransport)

	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &ReceiveResolving{}, state)

	// A timer-driven resume re-fires the phase-1 query from the parked
	// state alone.
	res = b.dispatch(ctx, &ResumeSessionRequest{
		SessionID:      sid,
		FromRetryTimer: true,
	})
	require.True(t, res.IsOk(), res.Err())

	require.Len(t, b.pendingTransport, 1)
	query := b.pendingTransport[0]
	_, ok := query.(*serverconn.SendListOORRecipientEventsByScriptRequest)
	require.True(t, ok)

	state, err = b.fsm.CurrentState()
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

	// A redelivered nil-ArkPSBT SubmitAcceptedEvent against the advanced
	// state must be a clean no-op ack, not an enrichment error: an
	// at-least-once operator can re-push the same accept after a reconnect.
	// Without this the duplicate would Nack and retry to the dead-letter
	// against the deterministic, durable FSM.
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

	// The FSM stays parked in AwaitingFinalizeAccepted and emits nothing.
	state, err = b.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &AwaitingFinalizeAccepted{}, state)
	require.Empty(t, b.pendingTransport)
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

// TestSessionActorStopFSMReapsGoroutine verifies stopFSM stops the running FSM
// (its driveMachine goroutine exits) and clears the reference, so the actor's
// teardown path does not leak the goroutine. The FSM is started on a
// non-cancelling context, so an explicit Stop is the only thing that reaps it.
func TestSessionActorStopFSMReapsGoroutine(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sid := oorSessionID(0x42)

	session, err := newReceiveSessionWithState(
		ctx, sid, &ReceiveResolving{
			SessionID: sid,
		},
	)
	require.NoError(t, err)
	require.True(t, session.FSM.IsRunning())

	b := &sessionBehavior{
		actorID:   ActorIDForSession(sid),
		log:       btclog.Disabled,
		sessionID: sid,
		fsm:       session.FSM,
	}

	b.stopFSM()

	require.False(t, session.FSM.IsRunning())
	require.Nil(t, b.fsm)

	// Idempotent: a second stop on the now-nil FSM is a no-op.
	b.stopFSM()
}

// TestSessionActorSetFSMStopsPrevious verifies setFSM stops the FSM it replaces
// so a reload (or re-admission) does not leak the stale FSM's goroutine.
func TestSessionActorSetFSMStopsPrevious(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sid := oorSessionID(0x43)

	prev, err := newReceiveSessionWithState(
		ctx, sid, &ReceiveResolving{
			SessionID: sid,
		},
	)
	require.NoError(t, err)
	next, err := newReceiveSessionWithState(
		ctx, sid, &ReceiveResolving{
			SessionID: sid,
		},
	)
	require.NoError(t, err)

	b := &sessionBehavior{
		actorID:   ActorIDForSession(sid),
		log:       btclog.Disabled,
		sessionID: sid,
		fsm:       prev.FSM,
	}

	b.setFSM(next.FSM)

	require.False(t, prev.FSM.IsRunning())
	require.True(t, next.FSM.IsRunning())
	require.Equal(t, next.FSM, b.fsm)

	b.stopFSM()
	require.False(t, next.FSM.IsRunning())
}

// fakeServerConnRef is a no-op serverconn tell target for behavior tests.
type fakeServerConnRef struct{}

func (fakeServerConnRef) ID() string {
	return "serverconn"
}

func (fakeServerConnRef) Tell(context.Context, serverconn.ServerConnMsg) error {
	return nil
}

// recordingServerConnRef captures every transport Tell so a test can assert
// the turn delivered (or withheld) its transport into the serverconn durable
// actor.
type recordingServerConnRef struct {
	mu   sync.Mutex
	msgs []serverconn.ServerConnMsg
}

func (*recordingServerConnRef) ID() string {
	return "serverconn"
}

func (r *recordingServerConnRef) Tell(_ context.Context,
	msg serverconn.ServerConnMsg) error {

	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, msg)

	return nil
}

func (r *recordingServerConnRef) recorded() []serverconn.ServerConnMsg {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]serverconn.ServerConnMsg(nil), r.msgs...)
}

// failingServerConnRef fails every transport Tell, modeling a serverconn
// durable-mailbox enqueue that errors inside the OOR commit transaction.
type failingServerConnRef struct {
	err error
}

func (failingServerConnRef) ID() string {
	return "serverconn"
}

func (r failingServerConnRef) Tell(context.Context,
	serverconn.ServerConnMsg) error {

	return r.err
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

// recordingLedgerSink records ledger Tells and whether each one arrived while
// the turn's commit closure was executing.
type recordingLedgerSink struct {
	inCommit *bool
	tellErr  error

	msgs         []ledger.LedgerMsg
	toldInCommit []bool
}

func (s *recordingLedgerSink) ID() string {
	return "ledger-test-sink"
}

func (s *recordingLedgerSink) Tell(_ context.Context,
	msg ledger.LedgerMsg) error {

	if s.tellErr != nil {
		return s.tellErr
	}

	s.msgs = append(s.msgs, msg)
	s.toldInCommit = append(s.toldInCommit, *s.inCommit)

	return nil
}

// commitTrackingExec flags when the commit closure is running so tests can
// assert a side effect executes inside the commit transaction.
type commitTrackingExec struct {
	fakeExec

	inCommit *bool
}

func (e commitTrackingExec) Commit(ctx context.Context,
	fn func(context.Context, oorTx) error) error {

	*e.inCommit = true
	defer func() {
		*e.inCommit = false
	}()

	return e.fakeExec.Commit(ctx, fn)
}

// TestSessionActorFinalizeLedgerTellInCommit verifies the outgoing-transfer
// ledger entry is delivered to the durable ledger sink inside the commit
// transaction (so a committed turn can never lose its accounting), and that a
// sink failure fails the whole turn instead of silently dropping the entry.
func TestSessionActorFinalizeLedgerTellInCommit(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := newFakeRegistryStore()
	b, sessionID, _ := finalizeBehavior(
		t, store, &fakePackageStore{},
		func(context.Context, []wire.OutPoint) error { return nil },
	)
	b.cfg.ServerConn = fakeServerConnRef{}

	var inCommit bool
	sink := &recordingLedgerSink{inCommit: &inCommit}
	b.cfg.LedgerSink = fn.Some[ledger.Sink](sink)

	ax := commitTrackingExec{
		fakeExec: fakeExec{tx: oorTx{
			store:   &fakeDeliveryStore{},
			actorID: b.actorID,
		}},
		inCommit: &inCommit,
	}

	res := b.Receive(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event:     &FinalizeAcceptedEvent{},
	}, ax)
	require.True(t, res.IsOk(), res.Err())

	// Exactly one VTXOSentMsg, told while the commit closure was running.
	require.Len(t, sink.msgs, 1)
	require.Equal(t, []bool{true}, sink.toldInCommit)

	sent, ok := sink.msgs[0].(*ledger.VTXOSentMsg)
	require.True(t, ok, "expected VTXOSentMsg, got %T", sink.msgs[0])
	require.Equal(t, [32]byte(sessionID), sent.SessionID)
	require.Positive(t, sent.AmountSat)
}

// TestSessionActorFinalizeLedgerTellFailureFailsTurn verifies a ledger sink
// failure inside the commit aborts the turn so the durable framework retries
// it, rather than committing the snapshot without its accounting.
func TestSessionActorFinalizeLedgerTellFailureFailsTurn(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := newFakeRegistryStore()
	b, sessionID, _ := finalizeBehavior(
		t, store, &fakePackageStore{},
		func(context.Context, []wire.OutPoint) error { return nil },
	)
	b.cfg.ServerConn = fakeServerConnRef{}

	var inCommit bool
	sink := &recordingLedgerSink{
		inCommit: &inCommit,
		tellErr:  fmt.Errorf("ledger mailbox unavailable"),
	}
	b.cfg.LedgerSink = fn.Some[ledger.Sink](sink)

	ax := commitTrackingExec{
		fakeExec: fakeExec{tx: oorTx{
			store:   &fakeDeliveryStore{},
			actorID: b.actorID,
		}},
		inCommit: &inCommit,
	}

	res := b.Receive(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event:     &FinalizeAcceptedEvent{},
	}, ax)
	require.True(t, res.IsErr())
	require.ErrorContains(t, res.Err(), "tell ledger")
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

// recordingTimeoutRef records timeout-actor scheduling requests.
type recordingTimeoutRef struct {
	mu   sync.Mutex
	msgs []timeout.Msg
}

func (r *recordingTimeoutRef) ID() string {
	return "timeout"
}

func (r *recordingTimeoutRef) Tell(_ context.Context, msg timeout.Msg) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.msgs = append(r.msgs, msg)

	return nil
}

// scheduled returns a copy of the recorded timeout messages.
func (r *recordingTimeoutRef) scheduled() []timeout.Msg {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]timeout.Msg(nil), r.msgs...)
}

// fakeExpiredRef is a no-op timeout expiry callback target.
type fakeExpiredRef struct{}

func (fakeExpiredRef) ID() string {
	return "oor-callback"
}

func (fakeExpiredRef) Tell(context.Context, *timeout.ExpiredMsg) error {
	return nil
}

// TestSessionActorResumeMetadataBackoff verifies resuming an incoming session
// parked in ReceiveNotified with persisted metadata attempts re-schedules the
// deterministic backoff instead of firing the metadata query immediately, so
// crash loops cannot burn through the retry budget or re-spin the operator
// mailbox.
func TestSessionActorResumeMetadataBackoff(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	timeoutRef := &recordingTimeoutRef{}

	ark, checkpoints := testOutboxPSBTPair(t)
	sid := oorSessionID(0x62)
	session, err := newReceiveSessionWithState(
		ctx, sid, &ReceiveNotified{
			SessionID:            sid,
			ArkPSBT:              ark,
			FinalCheckpointPSBTs: checkpoints,
			MetadataAttempts:     3,
		},
	)
	require.NoError(t, err)

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore: newFakeRegistryStore(),
			TimeoutActor:  timeoutRef,
			CallbackRef:   fakeExpiredRef{},
		},
		log:       btclog.Disabled,
		sessionID: sid,
		direction: clientdb.OORSessionDirectionIncoming,
		fsm:       session.FSM,
		loaded:    true,
	}

	res := b.Receive(ctx, &ResumeSessionRequest{SessionID: sid}, fakeExec{})
	require.True(t, res.IsOk(), res.Err())

	// No metadata query may be re-collected: a boot resume re-arms the
	// give-up timer with the next attempt's delay instead, and does not
	// advance the persisted MetadataAttempts.
	require.Empty(t, b.pendingTransport)

	scheduled := timeoutRef.scheduled()
	require.Len(t, scheduled, 1)

	schedule, ok := scheduled[0].(*timeout.ScheduleTimeoutRequest)
	require.True(t, ok)
	require.Equal(t, timeout.ID(sid.String()), schedule.ID)
	require.Equal(t, metadataRetryBackoff(4), schedule.Duration)

	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	notified, ok := state.(*ReceiveNotified)
	require.True(t, ok)
	require.Equal(t, uint32(3), notified.MetadataAttempts)
}

// failingUpsertStore wraps the fake registry store with an UpsertSession that
// always fails, so commitAck rolls the turn back.
type failingUpsertStore struct {
	*fakeRegistryStore
}

func (s *failingUpsertStore) UpsertSession(context.Context,
	clientdb.OORSessionRegistryRecord) error {

	return errFilterBroken
}

// TestSessionActorRetryArmsOnlyAfterCommit verifies a retry timer requested by
// the FSM is armed only once the turn commits: the dispatch phase queues it,
// and a rolled-back turn arms nothing.
func TestSessionActorRetryArmsOnlyAfterCommit(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	newBehavior := func(store SessionRegistryStore,
		timeoutRef *recordingTimeoutRef) *sessionBehavior {

		ark, checkpoints := testOutboxPSBTPair(t)
		sid := oorSessionID(0x63)
		session, err := newReceiveSessionWithState(
			ctx, sid, &ReceiveNotified{
				SessionID:            sid,
				ArkPSBT:              ark,
				FinalCheckpointPSBTs: checkpoints,
				MetadataAttempts:     2,
			},
		)
		require.NoError(t, err)

		return &sessionBehavior{
			cfg: SessionActorConfig{
				RegistryStore: store,
				TimeoutActor:  timeoutRef,
				CallbackRef:   fakeExpiredRef{},
			},
			log:       btclog.Disabled,
			sessionID: sid,
			direction: clientdb.OORSessionDirectionIncoming,
			fsm:       session.FSM,
			loaded:    true,
		}
	}

	// A turn whose commit fails must not arm the timer.
	rolledBack := &recordingTimeoutRef{}
	b := newBehavior(
		&failingUpsertStore{newFakeRegistryStore()}, rolledBack,
	)

	res := b.Receive(ctx, &ResumeSessionRequest{
		SessionID: b.sessionID,
	}, fakeExec{})
	require.True(t, res.IsErr())
	require.Empty(t, rolledBack.scheduled())

	// The same turn against a healthy store arms exactly one timer.
	committed := &recordingTimeoutRef{}
	b = newBehavior(newFakeRegistryStore(), committed)

	res = b.Receive(ctx, &ResumeSessionRequest{
		SessionID: b.sessionID,
	}, fakeExec{})
	require.True(t, res.IsOk(), res.Err())
	require.Len(t, committed.scheduled(), 1)
}

// countingReservationStore records how many reservation rows were written so a
// test can assert a deduped loser turn persists nothing.
type countingReservationStore struct {
	reservations int
}

func (s *countingReservationStore) UpsertReservation(context.Context,
	wire.OutPoint, int, chainhash.Hash) error {

	s.reservations++

	return nil
}

// TestSessionActorOutgoingAdmissionKeyDedupLoses verifies an outgoing admission
// that loses an idempotency-key race is converted into a clean Existing
// response for the surviving session instead of failing on the partial UNIQUE
// index. The loser must write nothing: no reservations and no snapshot row of
// its own.
func TestSessionActorOutgoingAdmissionKeyDedupLoses(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}
	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{clientKey}, nil,
	)

	inputValue := btcutil.Amount(10_000)
	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, policy.OperatorKey, wire.OutPoint{
				Hash:  [32]byte{0x07},
				Index: 0,
			},
			inputValue,
		),
	}
	recipients := []oortx.RecipientOutput{{
		PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
		Value:    inputValue,
	}}

	const key = "shared-idempotency-key"

	// Seed the surviving winner: a different session id carrying the same
	// idempotency key, as a concurrent same-key admission that committed
	// first would have left behind.
	store := newFakeRegistryStore()
	winnerID := oorSessionID(0xee)
	upsertRegistryRow(
		t, store, winnerID, clientdb.OORSessionDirectionOutgoing,
		string(OutgoingPhaseSubmitSent), key,
		clientdb.OORSessionStatusPending,
	)

	reservations := &countingReservationStore{}
	delivery := &fakeDeliveryStore{}
	conn := &recordingServerConnRef{}
	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore:    store,
			Signer:           clientSigner,
			ReservationStore: reservations,
			ServerConn:       conn,
		},
		actorID: "oor-loser",
		log:     btclog.Disabled,
	}

	ax := fakeExec{tx: oorTx{store: delivery, actorID: b.actorID}}

	// The loser's admission turn builds its own (different) session id,
	// signs, and reaches commit. The in-commit key lookup finds the winner
	// and the turn dedups to it.
	res := b.Receive(ctx, &StartTransferRequest{
		Policy:         policy,
		Inputs:         inputs,
		Recipients:     recipients,
		IdempotencyKey: key,
	}, ax)
	require.True(t, res.IsOk(), res.Err())

	resp, ok := res.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)
	require.True(t, resp.Existing)
	require.Equal(t, winnerID, resp.SessionID)

	// The loser is a different session id than the winner ...
	require.NotEqual(t, winnerID, b.sessionID)

	// ... and it persisted nothing: no reservations, no own snapshot row,
	// and no transport told to serverconn.
	require.Equal(t, 0, reservations.reservations)
	require.Empty(t, delivery.enqueued)
	require.Empty(t, conn.recorded())
	_, err = store.GetSession(ctx, chainHashOf(b.sessionID))
	require.ErrorIs(t, err, clientdb.ErrOORSessionNotFound)

	// The winner row is untouched.
	winnerRow, err := store.GetSession(ctx, chainHashOf(winnerID))
	require.NoError(t, err)
	require.Equal(t, key, winnerRow.IdempotencyKey)
}

// TestSessionActorFinalizeReloadsAfterSpendFailure pins the full-Receive
// regression for the inline-effect failure path: a FinalizeAcceptedEvent
// advances b.fsm past AwaitingFinalizeAccepted in memory before completeSpend
// runs, so a transient SpendCompleter failure leaves the in-memory FSM
// observably ahead of the last-committed AwaitingFinalizeAccepted row. The fix
// marks the turn dirty (b.commitFailed) so the redelivered turn reloads the FSM
// from the durable row and re-applies the full transition. Without the reload
// the redelivered FinalizeAcceptedEvent would find the FSM already past
// AwaitingFinalizeAccepted: captureFinalizeState would return nil and the
// finalized package write plus the vtxo_sent ledger entry -- both gated on that
// transition -- would be silently dropped.
func TestSessionActorFinalizeReloadsAfterSpendFailure(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Build an outgoing FSM parked at AwaitingFinalizeAccepted, and seed
	// the last durably-committed row at that same state so the reload after
	// the failed turn rebuilds the FSM there.
	ark, checkpoints := testOutboxPSBTPair(t)
	sessionID, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	inputs := testRetryTransferInputs(t)
	awaiting := &AwaitingFinalizeAccepted{
		SessionID:            sessionID,
		ArkPSBT:              ark,
		FinalCheckpointPSBTs: checkpoints,
		TransferInputs:       inputs,
	}
	awaitingRecord, err := outgoingRegistryRecord(sessionID, awaiting)
	require.NoError(t, err)

	store := newFakeRegistryStore()
	require.NoError(t, store.UpsertSession(ctx, awaitingRecord))

	snapshot, err := NewOutgoingSnapshot(sessionID, awaiting)
	require.NoError(t, err)

	session, err := NewSessionFromSnapshot(ctx, snapshot)
	require.NoError(t, err)

	// The completer fails the first spend and succeeds the second, modeling
	// a transient VTXO-manager failure that clears on redelivery.
	var spends int
	completer := func(context.Context, []wire.OutPoint) error {
		spends++
		if spends == 1 {
			return errFilterBroken
		}

		return nil
	}

	pkgStore := &fakePackageStore{}

	var inCommit bool
	sink := &recordingLedgerSink{inCommit: &inCommit}

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore:  store,
			PackageStore:   pkgStore,
			SpendCompleter: completer,
			ServerConn:     fakeServerConnRef{},
			LedgerSink:     fn.Some[ledger.Sink](sink),
		},
		actorID:   ActorIDForSession(sessionID),
		log:       btclog.Disabled,
		sessionID: sessionID,
		direction: clientdb.OORSessionDirectionOutgoing,
		fsm:       session.FSM,
		loaded:    true,
	}

	ax := commitTrackingExec{
		fakeExec: fakeExec{tx: oorTx{
			store:   &fakeDeliveryStore{},
			actorID: b.actorID,
		}},
		inCommit: &inCommit,
	}

	drive := &DriveEventRequest{
		SessionID: sessionID,
		Event:     &FinalizeAcceptedEvent{},
	}

	// First turn: the spend fails inline in dispatch after the FSM already
	// advanced, so the turn errors and the behavior marks itself dirty.
	res := b.Receive(ctx, drive, ax)
	require.True(t, res.IsErr())
	require.ErrorIs(t, res.Err(), errFilterBroken)
	require.Equal(t, 1, spends)
	require.True(t, b.commitFailed)

	// No terminal/advanced snapshot was committed: the durable row is still
	// the last-committed AwaitingFinalizeAccepted, the finalized package
	// was not persisted, and the vtxo_sent ledger entry never fired.
	stored, err := store.GetSession(ctx, chainHashOf(sessionID))
	require.NoError(t, err)
	require.False(t, stored.Status.IsTerminal())
	require.Equal(t, string(OutgoingPhaseFinalizeSent), stored.Phase)
	require.Equal(t, 0, pkgStore.packages)
	require.Empty(t, sink.msgs)

	// Redelivery: the reload guard rebuilds the FSM from the
	// AwaitingFinalizeAccepted row, so the same event re-runs the full
	// transition. This time the spend succeeds and the turn commits a
	// terminal Completed snapshot WITH the finalized package persisted and
	// the vtxo_sent ledger entry told inside the commit.
	res = b.Receive(ctx, drive, ax)
	require.True(t, res.IsOk(), res.Err())
	require.False(t, b.commitFailed)
	require.Equal(t, 2, spends)
	require.True(t, b.terminalCommitted)

	state, err := b.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &Completed{}, state)

	stored, err = store.GetSession(ctx, chainHashOf(sessionID))
	require.NoError(t, err)
	require.True(t, stored.Status.IsTerminal())

	// The finalized package was persisted and the ledger entry was told
	// inside the commit closure -- the work that the unfixed redelivered
	// turn would have lost.
	require.Equal(t, 1, pkgStore.packages)
	require.Len(t, sink.msgs, 1)
	require.Equal(t, []bool{true}, sink.toldInCommit)
	sent, ok := sink.msgs[0].(*ledger.VTXOSentMsg)
	require.True(t, ok, "expected VTXOSentMsg, got %T", sink.msgs[0])
	require.Equal(t, [32]byte(sessionID), sent.SessionID)
}
