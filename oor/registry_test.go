package oor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// recordingTellRef is a TellOnlyRef stub that records every message told to a
// stubbed child actor, failing when the recorder is configured to.
type recordingTellRef struct {
	id  string
	rec *registrySpawnRecorder
}

// ID returns the stub actor id.
func (r *recordingTellRef) ID() string {
	return r.id
}

// Tell records the message instead of enqueueing it.
func (r *recordingTellRef) Tell(_ context.Context, msg OORDurableMsg) error {
	if err := r.rec.tellErr; err != nil {
		return err
	}

	r.rec.record(msg)

	return nil
}

// recordingActorRef extends recordingTellRef with an Ask that completes from
// the recorder's configured admission result, modeling a child's first turn.
type recordingActorRef struct {
	recordingTellRef
}

// Ask records the message and completes immediately with the configured
// admission result.
func (r *recordingActorRef) Ask(_ context.Context,
	msg OORDurableMsg) actor.Future[ActorResp] {

	promise := actor.NewPromise[ActorResp]()
	if err := r.rec.askErr; err != nil {
		promise.Complete(fn.Err[ActorResp](err))

		return promise.Future()
	}

	r.rec.record(msg)
	promise.Complete(fn.Ok[ActorResp](&StartTransferResponse{}))

	return promise.Future()
}

// neverResolvingActorRef is an ActorRef stub whose Ask returns a future that
// never completes, modeling a wedged child admission turn. It lets a test
// assert the registry's detached-continuation wait is bounded rather than
// leaking the OnComplete goroutine forever under an uncancellable caller
// context.
type neverResolvingActorRef struct {
	recordingTellRef
}

// Ask returns a future that never resolves.
func (r *neverResolvingActorRef) Ask(_ context.Context,
	_ OORDurableMsg) actor.Future[ActorResp] {

	// The promise is never completed, so the only unblock for a
	// continuation parked on this future is its wait context being done.
	return actor.NewPromise[ActorResp]().Future()
}

// registrySpawnRecorder captures the children spawned by a registry behavior
// under test, every message the registry told them, and every child stop. The
// mutex makes the recorder safe for the asynchronous terminal notification
// goroutine.
type registrySpawnRecorder struct {
	mu     sync.Mutex
	spawns int
	stops  int
	dirs   []clientdb.OORSessionDirection
	tells  []OORDurableMsg

	// askErr fails every stub child Ask when set.
	askErr error

	// tellErr fails every stub child Tell when set.
	tellErr error
}

// record appends one told message.
func (r *registrySpawnRecorder) record(msg OORDurableMsg) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tells = append(r.tells, msg)
}

// recorded returns a copy of the told messages.
func (r *registrySpawnRecorder) recorded() []OORDurableMsg {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]OORDurableMsg(nil), r.tells...)
}

// stopCount returns the number of child stops observed.
func (r *registrySpawnRecorder) stopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.stops
}

// newTestRegistryBehavior builds a registry behavior backed by the given store
// with a recording spawn stub, so the coordination logic (dedup, restore,
// routing decisions) can be exercised without real durable child actors.
func newTestRegistryBehavior(store SessionRegistryStore) (*oorRegistryBehavior,
	*registrySpawnRecorder) {

	rec := &registrySpawnRecorder{}
	b := &oorRegistryBehavior{
		cfg: OORRegistryConfig{
			RegistryStore: store,
			Limits:        DefaultReceiveLimits(),
		},
		log:        btclog.Disabled,
		active:     make(map[SessionID]*OORSessionActor),
		activeDirs: make(map[SessionID]clientdb.OORSessionDirection),
		incoming:   make(map[SessionID]struct{}),
	}
	b.spawnFunc = func(id SessionID, dir clientdb.OORSessionDirection) (
		*OORSessionActor, error) {

		rec.spawns++
		rec.dirs = append(rec.dirs, dir)

		tellRef := &recordingTellRef{
			id:  ActorIDForSession(id),
			rec: rec,
		}

		return &OORSessionActor{
			ref: &recordingActorRef{
				recordingTellRef: *tellRef,
			},
			tellRef: tellRef,
			stop: func() {
				rec.mu.Lock()
				defer rec.mu.Unlock()

				rec.stops++
			},
		}, nil
	}

	return b, rec
}

// oorSessionID builds a SessionID from a seed byte.
func oorSessionID(seed byte) SessionID {
	var id SessionID
	id[0] = seed
	id[1] = 0xcd

	return id
}

// upsertRegistryRow writes one control-plane row, building the record at low
// indentation to keep the struct literal readable.
func upsertRegistryRow(t *testing.T, store *fakeRegistryStore, id SessionID,
	dir clientdb.OORSessionDirection, phase, key string,
	status clientdb.OORSessionStatus) {

	t.Helper()

	rec := clientdb.OORSessionRegistryRecord{
		SessionID:      chainHashOf(id),
		ActorID:        ActorIDForSession(id),
		Direction:      dir,
		Phase:          phase,
		IdempotencyKey: key,
		Status:         status,
		SnapshotData: []byte{
			0x01,
		},
	}
	require.NoError(t, store.UpsertSession(t.Context(), rec))
}

// TestOORRegistryRestoreNonTerminal verifies restore spawns one child per
// non-terminal row and skips terminal ones.
func TestOORRegistryRestoreNonTerminal(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newFakeRegistryStore()

	out := oorSessionID(0x01)
	in := oorSessionID(0x02)
	done := oorSessionID(0x03)

	upsertRegistryRow(
		t, store, out, clientdb.OORSessionDirectionOutgoing,
		"submit_sent", "", clientdb.OORSessionStatusPending,
	)
	upsertRegistryRow(
		t, store, in, clientdb.OORSessionDirectionIncoming,
		"resolve_pending", "", clientdb.OORSessionStatusPending,
	)
	upsertRegistryRow(
		t, store, done, clientdb.OORSessionDirectionOutgoing,
		"completed", "", clientdb.OORSessionStatusCompleted,
	)

	b, rec := newTestRegistryBehavior(store)
	require.NoError(t, b.restoreNonTerminal(ctx))

	require.Equal(t, 2, rec.spawns)
	require.Len(t, b.active, 2)
	require.Contains(t, b.active, out)
	require.Contains(t, b.active, in)
	require.NotContains(t, b.active, done)

	// Every restored child must receive exactly one resume so it re-drives
	// the outbox implied by its restored state instead of stalling until an
	// operator response that may never arrive.
	require.Len(t, rec.recorded(), 2)
	resumed := make(map[SessionID]int)
	for _, msg := range rec.recorded() {
		resume, ok := msg.(*ResumeSessionRequest)
		require.True(t, ok, "expected resume, got %T", msg)
		resumed[resume.SessionID]++
	}
	require.Equal(t, map[SessionID]int{out: 1, in: 1}, resumed)
}

// TestOORRegistryStartTransferDedup verifies a StartTransfer carrying an
// idempotency key that already exists returns the existing session without
// spawning a child.
func TestOORRegistryStartTransferDedup(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newFakeRegistryStore()

	existing := oorSessionID(0x10)
	upsertRegistryRow(
		t, store, existing, clientdb.OORSessionDirectionOutgoing,
		"submit_sent", "key-1", clientdb.OORSessionStatusPending,
	)

	b, rec := newTestRegistryBehavior(store)

	res := b.handleStartTransfer(ctx, &StartTransferRequest{
		IdempotencyKey: "key-1",
	})
	require.True(t, res.IsOk(), res.Err())

	resp, ok := res.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)
	require.True(t, resp.Existing)
	require.Equal(t, existing, resp.SessionID)
	require.Equal(t, 0, rec.spawns)
}

// TestOORRegistryStartTransferRetryAfterFailure verifies a failed keyed
// session never dedups a retry: admission proceeds past the idempotency
// lookup instead of echoing the dead session as Existing.
func TestOORRegistryStartTransferRetryAfterFailure(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newFakeRegistryStore()

	failed := oorSessionID(0x11)
	upsertRegistryRow(
		t, store, failed, clientdb.OORSessionDirectionOutgoing,
		"failed", "key-1", clientdb.OORSessionStatusFailed,
	)

	b, _ := newTestRegistryBehavior(store)

	// The lookup must skip the failed row, so admission falls through to
	// deterministic session construction, which rejects this empty request
	// instead of returning the dead session as Existing.
	res := b.handleStartTransfer(ctx, &StartTransferRequest{
		IdempotencyKey: "key-1",
	})
	require.True(t, res.IsErr())
}

// TestOORRegistryListSessions verifies the coarse list projects the
// control-plane rows -- including terminal/failed ones with their failure
// reason -- and honours the direction and pending-only filters.
func TestOORRegistryListSessions(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newFakeRegistryStore()

	out := oorSessionID(0x20)
	in := oorSessionID(0x21)
	failed := oorSessionID(0x22)
	upsertRegistryRow(
		t, store, out, clientdb.OORSessionDirectionOutgoing,
		"submit_sent", "", clientdb.OORSessionStatusPending,
	)
	upsertRegistryRow(
		t, store, in, clientdb.OORSessionDirectionIncoming,
		"resolve_pending", "", clientdb.OORSessionStatusPending,
	)

	failedRec := clientdb.OORSessionRegistryRecord{
		SessionID: chainHashOf(failed),
		ActorID:   ActorIDForSession(failed),
		Direction: clientdb.OORSessionDirectionOutgoing,
		Phase:     "failed",
		Status:    clientdb.OORSessionStatusFailed,
		LastError: "server rejected",
	}
	require.NoError(t, store.UpsertSession(ctx, failedRec))

	b, _ := newTestRegistryBehavior(store)

	// The unfiltered list includes the failed session with its terminal
	// diagnostics.
	all := b.handleListSessions(ctx, &ListSessionsRequest{
		Direction: SessionDirectionAll,
	})
	require.True(t, all.IsOk())
	allResp, ok := all.UnwrapOr(nil).(*ListSessionsResponse)
	require.True(t, ok)
	require.Len(t, allResp.Sessions, 3)

	var failedSummary *SessionSummary
	for i := range allResp.Sessions {
		if allResp.Sessions[i].SessionID == failed {
			failedSummary = &allResp.Sessions[i]
		}
	}
	require.NotNil(t, failedSummary)
	require.False(t, failedSummary.Pending)
	require.Equal(t, "server rejected", failedSummary.RetryReason)

	outgoing := b.handleListSessions(ctx, &ListSessionsRequest{
		Direction: SessionDirectionOutgoing,
	})
	require.True(t, outgoing.IsOk())
	outResp, ok := outgoing.UnwrapOr(nil).(*ListSessionsResponse)
	require.True(t, ok)
	require.Len(t, outResp.Sessions, 2)
	for i := range outResp.Sessions {
		require.Equal(
			t, SessionDirectionOutgoing,
			outResp.Sessions[i].Direction,
		)
	}

	// PendingOnly drops terminal rows.
	pendingOnly := b.handleListSessions(ctx, &ListSessionsRequest{
		Direction:   SessionDirectionAll,
		PendingOnly: true,
	})
	require.True(t, pendingOnly.IsOk())
	pendingResp, ok := pendingOnly.UnwrapOr(nil).(*ListSessionsResponse)
	require.True(t, ok)
	require.Len(t, pendingResp.Sessions, 2)
	for i := range pendingResp.Sessions {
		require.True(t, pendingResp.Sessions[i].Pending)
	}
}

// TestOORRegistryListSessionsSnapshotProjection verifies an outgoing session's
// consumed inputs are projected from the persisted snapshot for diagnostics.
func TestOORRegistryListSessionsSnapshotProjection(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newFakeRegistryStore()

	// Persist a real outgoing snapshot via the behavior bridge so the
	// listing decodes exactly what production writes.
	b, sessionID, wantOutpoints := finalizeBehavior(
		t, store, &fakePackageStore{}, nil,
	)
	record, err := b.snapshotRecord()
	require.NoError(t, err)
	require.NoError(t, store.UpsertSession(ctx, record))

	reg, _ := newTestRegistryBehavior(store)

	res := reg.handleListSessions(ctx, &ListSessionsRequest{
		Direction: SessionDirectionAll,
	})
	require.True(t, res.IsOk(), res.Err())

	resp, ok := res.UnwrapOr(nil).(*ListSessionsResponse)
	require.True(t, ok)
	require.Len(t, resp.Sessions, 1)

	summary := resp.Sessions[0]
	require.Equal(t, sessionID, summary.SessionID)
	require.ElementsMatch(t, wantOutpoints, summary.InputOutpoints)
	require.Positive(t, summary.InputAmountSat)
}

// TestOORRegistryResumeRouting verifies the registry routes a retry-timer
// ResumeSessionRequest to its session's child, restoring the child from the
// control-plane store when needed, and treats resumes for unknown or terminal
// sessions as benign no-ops.
func TestOORRegistryResumeRouting(t *testing.T) {
	t.Parallel()

	pending := oorSessionID(0x30)
	terminal := oorSessionID(0x31)
	unknown := oorSessionID(0x32)

	testCases := []struct {
		name       string
		sessionID  SessionID
		wantSpawns int
		wantTells  int
	}{{
		// A resume for a non-terminal session not yet in memory must
		// restore the child and forward the resume.
		name:       "restores and routes pending session",
		sessionID:  pending,
		wantSpawns: 1,
		wantTells:  1,
	}, {
		// A timer may fire after its session completed; the resume is
		// dropped without spawning anything.
		name:      "drops resume for terminal session",
		sessionID: terminal,
	}, {
		// A timer may carry a session id with no row at all (e.g. an
		// unparseable timeout id); the resume is dropped.
		name:      "drops resume for unknown session",
		sessionID: unknown,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			store := newFakeRegistryStore()
			upsertRegistryRow(
				t, store, pending,
				clientdb.OORSessionDirectionOutgoing,
				"submit_sent", "",
				clientdb.OORSessionStatusPending,
			)
			upsertRegistryRow(
				t, store, terminal,
				clientdb.OORSessionDirectionOutgoing,
				"completed", "",
				clientdb.OORSessionStatusCompleted,
			)

			b, rec := newTestRegistryBehavior(store)

			res := b.Receive(ctx, &ResumeSessionRequest{
				SessionID: tc.sessionID,
			}, fakeExec{})
			require.True(t, res.IsOk(), res.Err())

			require.Equal(t, tc.wantSpawns, rec.spawns)
			require.Len(t, rec.recorded(), tc.wantTells)

			for _, msg := range rec.recorded() {
				resume, ok := msg.(*ResumeSessionRequest)
				require.True(t, ok)
				require.Equal(t, tc.sessionID, resume.SessionID)
			}
		})
	}
}

// TestOORRegistryResumeRoutingActiveChild verifies a resume for a session whose
// child is already in memory is forwarded without a second spawn.
func TestOORRegistryResumeRoutingActiveChild(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newFakeRegistryStore()

	id := oorSessionID(0x33)
	upsertRegistryRow(
		t, store, id, clientdb.OORSessionDirectionOutgoing,
		"submit_sent", "", clientdb.OORSessionStatusPending,
	)

	b, rec := newTestRegistryBehavior(store)

	// First resume spawns the child; the second reuses it.
	for range 2 {
		res := b.Receive(
			ctx, &ResumeSessionRequest{
				SessionID: id,
			},
			fakeExec{},
		)
		require.True(t, res.IsOk(), res.Err())
	}

	require.Equal(t, 1, rec.spawns)
	require.Len(t, rec.recorded(), 2)
}

// TestOORRegistrySessionTerminalReap verifies a terminal notification stops
// the child and drops it from the active set, re-checking the durable row as
// the authority so stale notifications cannot reap a live session.
func TestOORRegistrySessionTerminalReap(t *testing.T) {
	t.Parallel()

	id := oorSessionID(0x40)

	testCases := []struct {
		name string

		// rowStatus is the durable row status at notify time; nil means
		// no row exists.
		rowStatus *clientdb.OORSessionStatus

		wantReaped bool
	}{{
		name:       "terminal row reaps the child",
		rowStatus:  statusPtr(clientdb.OORSessionStatusCompleted),
		wantReaped: true,
	}, {
		name:       "failed row reaps the child",
		rowStatus:  statusPtr(clientdb.OORSessionStatusFailed),
		wantReaped: true,
	}, {
		name:       "missing row reaps the child",
		wantReaped: true,
	}, {
		name:      "stale notification keeps a live child",
		rowStatus: statusPtr(clientdb.OORSessionStatusPending),
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			store := newFakeRegistryStore()
			if tc.rowStatus != nil {
				upsertRegistryRow(
					t, store, id,
					clientdb.OORSessionDirectionOutgoing,
					"completed", "", *tc.rowStatus,
				)
			}

			b, rec := newTestRegistryBehavior(store)

			// Park a child in the active set directly.
			child, err := b.ensureChild(
				id, clientdb.OORSessionDirectionOutgoing,
			)
			require.NoError(t, err)
			require.NotNil(t, child)

			res := b.Receive(ctx, &SessionTerminalNotification{
				SessionID: id,
			}, fakeExec{})
			require.True(t, res.IsOk(), res.Err())

			if tc.wantReaped {
				require.NotContains(t, b.active, id)
				require.Equal(t, 1, rec.stopCount())

				return
			}

			require.Contains(t, b.active, id)
			require.Equal(t, 0, rec.stopCount())
		})
	}
}

// TestOORRegistryStopSkipsChildrenOnDrainTimeout verifies the shutdown race
// fix: stopChildren runs only when the registry drain succeeds (process() has
// provably exited). On a drain timeout the registry goroutine may still be
// running a wedged turn that mutates r.active, so stopChildren must be skipped
// to avoid a fatal concurrent map iteration and write; the children are left to
// actor-system shutdown.
func TestOORRegistryStopSkipsChildrenOnDrainTimeout(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// A drain timeout must skip stopChildren.
	b, rec := newTestRegistryBehavior(newFakeRegistryStore())
	child, err := b.ensureChild(
		oorSessionID(0x01), clientdb.OORSessionDirectionOutgoing,
	)
	require.NoError(t, err)
	require.NotNil(t, child)

	a := &OORRegistryActor{behavior: b}
	a.stopWithDrain(ctx, func(context.Context) error {
		return context.DeadlineExceeded
	})
	require.Equal(t, 0, rec.stopCount())

	// A clean drain stops every child.
	b2, rec2 := newTestRegistryBehavior(newFakeRegistryStore())
	child, err = b2.ensureChild(
		oorSessionID(0x02), clientdb.OORSessionDirectionOutgoing,
	)
	require.NoError(t, err)
	require.NotNil(t, child)

	a2 := &OORRegistryActor{behavior: b2}
	a2.stopWithDrain(ctx, func(context.Context) error {
		return nil
	})
	require.Equal(t, 1, rec2.stopCount())
}

// TestOORRegistryDriveEventTerminalSessionIsNoOp verifies a late-redelivered
// duplicate drive-event for a session that has reached a terminal snapshot and
// been reaped is absorbed as an idempotent no-op (acks cleanly), while a
// genuinely-unknown session still errors. Without this, a normal at-least-once
// duplicate would Nack, retry to the cap, and dead-letter.
func TestOORRegistryDriveEventTerminalSessionIsNoOp(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := newFakeRegistryStore()
	b, rec := newTestRegistryBehavior(store)

	// A terminal row with no resident child: a duplicate push must ack
	// cleanly without spawning a child.
	terminalID := oorSessionID(0x55)
	upsertRegistryRow(
		t, store, terminalID, clientdb.OORSessionDirectionIncoming,
		"completed", "", clientdb.OORSessionStatusCompleted,
	)

	res := b.handleDriveEvent(ctx, &DriveEventRequest{
		SessionID: terminalID,
		Event:     &FinalizeAcceptedEvent{},
	})
	require.True(t, res.IsOk(), res.Err())
	require.NotContains(t, b.active, terminalID)
	require.Equal(t, 0, rec.spawns)

	// A genuinely-unknown session is still an error.
	res = b.handleDriveEvent(ctx, &DriveEventRequest{
		SessionID: oorSessionID(0x56),
		Event:     &FinalizeAcceptedEvent{},
	})
	require.True(t, res.IsErr())
	require.Contains(t, res.Err().Error(), "unknown session")
}

// TestOORRegistrySessionTerminalUnknownChild verifies a terminal notification
// for a session with no resident child is a no-op.
func TestOORRegistrySessionTerminalUnknownChild(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b, rec := newTestRegistryBehavior(newFakeRegistryStore())

	res := b.Receive(ctx, &SessionTerminalNotification{
		SessionID: oorSessionID(0x41),
	}, fakeExec{})
	require.True(t, res.IsOk(), res.Err())
	require.Equal(t, 0, rec.stopCount())
}

// statusPtr returns a pointer to the given session status for table tests.
func statusPtr(status clientdb.OORSessionStatus) *clientdb.OORSessionStatus {
	return &status
}

// testSigner returns a mock signer so registry-construction validation passes
// in tests that exercise paths which never actually sign.
func testSigner(t *testing.T) input.Signer {
	t.Helper()

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return input.NewMockSigner([]*btcec.PrivateKey{key}, nil)
}

// fakeRecipientFilter is an OutboxHandler stub whose metadata recipient filter
// either owns every recipient or none of them.
type fakeRecipientFilter struct {
	owned bool
	err   error
}

// Handle satisfies OutboxHandler; admission validation never invokes it.
func (f *fakeRecipientFilter) Handle(context.Context, SessionID, OutboxEvent) (
	[]Event, error) {

	return nil, nil
}

// FilterIncomingMetadataRecipients returns all or none of the recipients.
func (f *fakeRecipientFilter) FilterIncomingMetadataRecipients(
	_ context.Context, recipients []ArkRecipientOutput) (
	[]ArkRecipientOutput, error) {

	if f.err != nil {
		return nil, f.err
	}
	if !f.owned {
		return nil, nil
	}

	return recipients, nil
}

// TestOORRegistryIncomingAdmissionOwnership verifies the registry rejects
// incoming hints whose recipient script the wallet does not own before
// spawning a per-session child, and admits owned recipients.
func TestOORRegistryIncomingAdmissionOwnership(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		handler    OutboxHandler
		wantErr    string
		wantSpawns int
	}{{
		name: "owned recipient is admitted",
		handler: &fakeRecipientFilter{
			owned: true,
		},
		wantSpawns: 1,
	}, {
		name: "unowned recipient is rejected before spawn",
		handler: &fakeRecipientFilter{
			owned: false,
		},
		wantErr: "not owned",
	}, {
		name: "filter failure rejects admission",
		handler: &fakeRecipientFilter{
			err: errFilterBroken,
		},
		wantErr: "filter broken",
	}, {
		// A handler without the filter view (or none at all) skips the
		// ownership gate, preserving harness setups that wire no
		// incoming handler.
		name:       "missing filter skips the gate",
		wantSpawns: 1,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			b, rec := newTestRegistryBehavior(
				newFakeRegistryStore(),
			)
			b.cfg.IncomingHandler = tc.handler

			res := b.Receive(ctx, &ResolveIncomingTransferRequest{
				SessionID:         oorSessionID(0x50),
				RecipientPkScript: []byte{0x51, 0x20, 0xaa},
			}, fakeExec{})

			if tc.wantErr != "" {
				require.True(t, res.IsErr())
				require.ErrorContains(t, res.Err(), tc.wantErr)
				require.Equal(t, 0, rec.spawns)

				return
			}

			require.True(t, res.IsOk(), res.Err())
			require.Equal(t, tc.wantSpawns, rec.spawns)
		})
	}
}

// TestOORRegistryIncomingAdmissionCap verifies the registry bounds the number
// of resident incoming children: an operator streaming distinct fabricated
// session ids over one owned receive script cannot pin unbounded children.
// Admissions up to the cap succeed; the next new session is rejected before a
// child is spawned, a resident session forwarding a follow-up hint is exempt,
// and reaping a session frees a slot for a fresh admission.
func TestOORRegistryIncomingAdmissionCap(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	const maxIncoming = 3

	b, rec := newTestRegistryBehavior(newFakeRegistryStore())
	b.cfg.IncomingHandler = &fakeRecipientFilter{owned: true}
	b.cfg.Limits.MaxConcurrentIncomingSessions = maxIncoming

	ownedScript := []byte{0x51, 0x20, 0xaa, 0xbb}
	admit := func(seed byte) fn.Result[ActorResp] {
		return b.Receive(ctx, &ResolveIncomingTransferRequest{
			SessionID:         oorSessionID(seed),
			RecipientPkScript: ownedScript,
		}, fakeExec{})
	}

	// The first maxIncoming admissions all spawn a child over the same
	// owned script.
	for i := 0; i < maxIncoming; i++ {
		res := admit(byte(i))
		require.True(t, res.IsOk(), res.Err())
	}
	require.Equal(t, maxIncoming, rec.spawns)
	require.Len(t, b.incoming, maxIncoming)

	// The next distinct session is rejected at the cap before any spawn.
	res := admit(byte(maxIncoming))
	require.True(t, res.IsErr())
	require.ErrorIs(t, res.Err(), errIncomingAdmissionCapped)
	require.Equal(t, maxIncoming, rec.spawns)

	// A resident session forwarding a follow-up hint is exempt: it already
	// counts against the cap, so it must not be rejected.
	res = admit(0x00)
	require.True(t, res.IsOk(), res.Err())
	require.Equal(t, maxIncoming, rec.spawns)

	// Reaping one session frees a slot, so a fresh admission succeeds.
	reaped := oorSessionID(0x00)
	b.dropChild(reaped, b.active[reaped])
	require.Len(t, b.incoming, maxIncoming-1)

	res = admit(byte(maxIncoming + 1))
	require.True(t, res.IsOk(), res.Err())
	require.Equal(t, maxIncoming+1, rec.spawns)
	require.Len(t, b.incoming, maxIncoming)
}

// TestOORRegistryEnsureChildEnforcesIncomingCap verifies the concurrency cap is
// enforced in ensureChild itself, the choke point every resident-making path
// funnels through (admission, lazy restore on a routed message, boot restore),
// so the bound holds even on the paths that skip handleResolveIncoming's
// pre-spawn check. Outgoing sessions carry no cap and are unaffected.
func TestOORRegistryEnsureChildEnforcesIncomingCap(t *testing.T) {
	t.Parallel()

	const maxIncoming = 2

	b, rec := newTestRegistryBehavior(newFakeRegistryStore())
	b.cfg.Limits.MaxConcurrentIncomingSessions = maxIncoming

	// Fill the incoming slots straight through ensureChild.
	for i := 0; i < maxIncoming; i++ {
		_, err := b.ensureChild(
			oorSessionID(
				byte(i),
			),
			clientdb.OORSessionDirectionIncoming,
		)
		require.NoError(t, err)
	}
	require.Len(t, b.incoming, maxIncoming)

	// A further new incoming child is rejected at the cap, with no spawn.
	spawnsAtCap := rec.spawns
	_, err := b.ensureChild(
		oorSessionID(
			byte(maxIncoming),
		),
		clientdb.OORSessionDirectionIncoming,
	)
	require.ErrorIs(t, err, errIncomingAdmissionCapped)
	require.Equal(t, spawnsAtCap, rec.spawns)

	// An outgoing child is exempt from the incoming cap.
	_, err = b.ensureChild(
		oorSessionID(0xf0), clientdb.OORSessionDirectionOutgoing,
	)
	require.NoError(t, err)
}

// errFilterBroken models a recipient filter infrastructure failure.
var errFilterBroken = errors.New("filter broken")

// TestOORRegistrySelfTransfer verifies the self-transfer invariant: an
// incoming hint for a session that exists as an outgoing session is deferred
// (errors) until the outgoing session reaches a terminal state, after which
// the outgoing entry is replaced by a fresh incoming session.
func TestOORRegistrySelfTransfer(t *testing.T) {
	t.Parallel()

	id := oorSessionID(0x70)

	testCases := []struct {
		name string

		// rowStatus is the outgoing row's status; nil means no row.
		rowStatus *clientdb.OORSessionStatus

		// residentChild parks an outgoing child in the active set
		// before delivering the hint.
		residentChild bool

		wantErr string

		// wantSpawnDirs are the spawn directions expected across the
		// whole test, including the optional resident child.
		wantSpawnDirs []clientdb.OORSessionDirection

		wantStops int
		wantTells int
	}{{
		name:      "active outgoing session defers the hint",
		rowStatus: statusPtr(clientdb.OORSessionStatusPending),
		wantErr:   "still active",
	}, {
		name:      "terminal outgoing row is replaced",
		rowStatus: statusPtr(clientdb.OORSessionStatusCompleted),
		wantSpawnDirs: []clientdb.OORSessionDirection{
			clientdb.OORSessionDirectionIncoming,
		},
		wantTells: 1,
	}, {
		name:          "resident terminal child stopped and replaced",
		rowStatus:     statusPtr(clientdb.OORSessionStatusCompleted),
		residentChild: true,
		wantSpawnDirs: []clientdb.OORSessionDirection{
			clientdb.OORSessionDirectionOutgoing,
			clientdb.OORSessionDirectionIncoming,
		},
		wantStops: 1,
		wantTells: 1,
	}, {
		name: "fresh session id admits normally",
		wantSpawnDirs: []clientdb.OORSessionDirection{
			clientdb.OORSessionDirectionIncoming,
		},
		wantTells: 1,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			store := newFakeRegistryStore()
			if tc.rowStatus != nil {
				upsertRegistryRow(
					t, store, id,
					clientdb.OORSessionDirectionOutgoing,
					"submit_sent", "", *tc.rowStatus,
				)
			}

			b, rec := newTestRegistryBehavior(store)
			if tc.residentChild {
				_, err := b.ensureChild(
					id,
					clientdb.OORSessionDirectionOutgoing,
				)
				require.NoError(t, err)
			}

			res := b.Receive(ctx, &ResolveIncomingTransferRequest{
				SessionID:         id,
				RecipientPkScript: []byte{0x51, 0x20, 0xbb},
			}, fakeExec{})

			if tc.wantErr != "" {
				require.True(t, res.IsErr())
				require.ErrorContains(t, res.Err(), tc.wantErr)

				// The resident-child count is unchanged and no
				// hint was forwarded.
				require.Empty(t, rec.recorded())

				return
			}

			require.True(t, res.IsOk(), res.Err())
			require.Equal(t, tc.wantSpawnDirs, rec.dirs)
			require.Equal(t, tc.wantStops, rec.stopCount())
			require.Len(t, rec.recorded(), tc.wantTells)
		})
	}
}

// TestOORRegistrySelfTransferParkAndRedrive verifies the event-driven defer
// path: a hint deferred by an active outgoing session is parked, the outgoing
// session's terminal notification redrives it as a self-Tell, and the
// redriven admission both succeeds and clears the parked entry.
func TestOORRegistrySelfTransferParkAndRedrive(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	id := oorSessionID(0x71)

	store := newFakeRegistryStore()
	upsertRegistryRow(
		t, store, id, clientdb.OORSessionDirectionOutgoing,
		"submit_sent", "", clientdb.OORSessionStatusPending,
	)

	b, rec := newTestRegistryBehavior(store)
	selfRec := &registrySpawnRecorder{}
	b.selfRef = &recordingTellRef{id: "registry-self", rec: selfRec}

	hint := &ResolveIncomingTransferRequest{
		SessionID: id,
		RecipientPkScript: []byte{
			0x51,
			0x20,
			0xbb,
		},
	}

	// While the outgoing session is active, the hint defers and parks.
	res := b.Receive(ctx, hint, fakeExec{})
	require.True(t, res.IsErr())
	require.ErrorIs(t, res.Err(), errSelfTransferDeferred)
	require.Len(t, b.parkedSelfHints, 1)
	require.Empty(t, rec.recorded())

	// A terminal notification before the row flips is a no-op for the
	// park: admission re-checks the row on the redriven copy, so the
	// premature redrive simply re-parks. Flip the row terminal first to
	// exercise the real sequence.
	upsertRegistryRow(
		t, store, id, clientdb.OORSessionDirectionOutgoing, "completed",
		"", clientdb.OORSessionStatusCompleted,
	)

	res = b.Receive(ctx, &SessionTerminalNotification{
		SessionID: id,
	}, fakeExec{})
	require.True(t, res.IsOk(), res.Err())

	// The reap redrove the parked hint into the registry's own mailbox
	// and cleared the park.
	require.Empty(t, b.parkedSelfHints)
	redriven := selfRec.recorded()
	require.Len(t, redriven, 1)
	require.Same(t, hint, redriven[0])

	// The redriven copy now admits an incoming session in place of the
	// terminal outgoing row.
	res = b.Receive(ctx, hint, fakeExec{})
	require.True(t, res.IsOk(), res.Err())
	require.Equal(
		t,
		[]clientdb.OORSessionDirection{
			clientdb.OORSessionDirectionIncoming,
		},
		rec.dirs,
	)
	require.Empty(t, b.parkedSelfHints)
}

// TestOORRegistryStaleTerminalDoesNotReapReplacementIncoming verifies that an
// outgoing terminal notification racing an incoming self-transfer replacement
// cannot reap the fresh incoming child before it commits its first snapshot.
func TestOORRegistryStaleTerminalDoesNotReapReplacementIncoming(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	id := oorSessionID(0x72)

	store := newFakeRegistryStore()
	upsertRegistryRow(
		t, store, id, clientdb.OORSessionDirectionOutgoing, "completed",
		"", clientdb.OORSessionStatusCompleted,
	)

	b, rec := newTestRegistryBehavior(store)
	child, err := b.ensureChild(id, clientdb.OORSessionDirectionIncoming)
	require.NoError(t, err)
	require.NotNil(t, child)

	res := b.Receive(ctx, &SessionTerminalNotification{
		SessionID: id,
	}, fakeExec{})
	require.True(t, res.IsOk(), res.Err())

	require.Contains(t, b.active, id)
	require.Equal(
		t, clientdb.OORSessionDirectionIncoming, b.activeDirs[id],
	)
	require.Equal(t, 0, rec.stopCount())
}

// TestOORRegistryDurableEndToEnd exercises the durable registry against a
// real sqlite-backed delivery store. This is the H-1 boundary regression
// test: a server-push Tell returns only after the message is persisted in the
// registry's durable inbox (so ingress may safely ack the operator), the
// registry turn spawns the per-session durable child and forwards the hint
// into its mailbox, and the child's admission turn commits the control-plane
// row.
func TestOORRegistryDurableEndToEnd(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	system := actor.NewActorSystem()
	defer func() {
		require.NoError(t, system.Shutdown(context.Background()))
	}()

	store := newFakeRegistryStore()
	registry, err := NewOORRegistryActor(OORRegistryConfig{
		RegistryStore:    store,
		DeliveryStore:    newTestDeliveryStore(t),
		ServerConn:       fakeServerConnRef{},
		Signer:           testSigner(t),
		IncomingHandler:  &fakeRecipientFilter{owned: true},
		PackageStore:     &fakePackageStore{},
		ReservationStore: &countingReservationStore{},
		ActorSystem:      system,
	})
	require.NoError(t, err)
	defer registry.Stop()

	// The boot-time restore runs as a registry message, serialized with
	// any redelivered backlog on the registry goroutine.
	require.NoError(t, registry.RestoreNonTerminal(ctx))

	// Deliver an incoming hint exactly as the EventRouter would: a Tell
	// that persists in the registry's durable inbox before returning.
	sid := oorSessionID(0x80)
	err = registry.Ref().Tell(ctx, &ResolveIncomingTransferRequest{
		SessionID:         sid,
		RecipientPkScript: []byte{0x51, 0x20, 0xaa, 0xbb},
		RecipientEventID:  3,
	})
	require.NoError(t, err)

	// The registry routes the hint to a freshly spawned child, whose
	// admission turn commits the control-plane row.
	require.Eventually(t, func() bool {
		record, err := store.GetSession(ctx, chainHashOf(sid))
		if err != nil {
			return false
		}

		incoming := clientdb.OORSessionDirectionIncoming

		return record.Direction == incoming &&
			record.Phase == string(IncomingPhaseResolvePending)
	}, 5*time.Second, 20*time.Millisecond)

	// A state probe Asks through the registry into the resident child.
	res := registry.Ref().Ask(ctx, &GetStateRequest{
		SessionID: sid,
	}).Await(ctx)
	require.True(t, res.IsOk(), res.Err())

	stateResp, ok := res.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &ReceiveResolving{}, stateResp.State)

	// The live child is registered under its per-session service key so
	// the ingress fast path can address its durable mailbox directly.
	refs := actor.FindInReceptionist(
		system.Receptionist(), SessionServiceKey(sid),
	)
	require.Len(t, refs, 1)
}

// TestOORRegistryFailedAdmissionDropsPhantomChild verifies a StartTransfer
// whose admission turn fails does not leave a phantom child in the active
// set: the failed child is dropped so a retry of the same transfer admits
// cleanly instead of being deduped against a session with no durable backing.
func TestOORRegistryFailedAdmissionDropsPhantomChild(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inputs := testRetryTransferInputs(t)
	req := &StartTransferRequest{
		Policy: arkscript.CheckpointPolicy{
			OperatorKey: operatorKey.PubKey(),
			CSVDelay:    10,
		},
		Inputs: inputs,
		Recipients: []oortx.RecipientOutput{{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputs[0].VTXO.Amount,
		}},
	}

	b, rec := newTestRegistryBehavior(newFakeRegistryStore())

	// First attempt: the child's admission turn fails. The child must be
	// dropped, not retained as a phantom.
	rec.askErr = errors.New("signer unavailable")

	res := b.Receive(ctx, req, fakeExec{})
	require.True(t, res.IsErr())
	require.Empty(t, b.active)
	require.Equal(t, 1, rec.spawns)
	require.Equal(t, 1, rec.stopCount())

	// Retry: with the failure cleared, the same transfer admits with a
	// fresh child instead of a phantom Existing response.
	rec.askErr = nil

	res = b.Receive(ctx, req, fakeExec{})
	require.True(t, res.IsOk(), res.Err())

	resp, ok := res.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)
	require.False(t, resp.Existing)
	require.Equal(t, 2, rec.spawns)
	require.Len(t, b.active, 1)
}

// TestOORRegistryAdmissionHandoffDeferredUntilCommit verifies the
// caller-promise handoff is wired only after the registry's consuming Commit
// succeeds: a lease-lost consume-commit surfaces the error, runs no inline
// admission, and leaves no orphaned handoff. Wiring the handoff before the
// Commit would race the framework's own promise completion when that Commit
// fails and the routing message redelivers.
func TestOORRegistryAdmissionHandoffDeferredUntilCommit(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inputs := testRetryTransferInputs(t)
	req := &StartTransferRequest{
		Policy: arkscript.CheckpointPolicy{
			OperatorKey: operatorKey.PubKey(),
			CSVDelay:    10,
		},
		Inputs: inputs,
		Recipients: []oortx.RecipientOutput{{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputs[0].VTXO.Amount,
		}},
	}

	b, rec := newTestRegistryBehavior(newFakeRegistryStore())

	// The child's admission would fail, but the consuming Commit loses the
	// lease first. Because the handoff is wired only after a successful
	// Commit, the admission is never completed inline on this turn: the
	// child is left resident for the redelivered turn to re-drive, and the
	// framework owns the single caller completion with the lease-lost
	// error. With the buggy ordering (handoff completed before the Commit),
	// the inline await would observe the admission failure and reap the
	// child here, which must not happen for a turn that did not commit.
	rec.askErr = errors.New("signer unavailable")

	res := b.Receive(ctx, req, leaseLostExec{})
	require.True(t, res.IsErr())
	require.ErrorIs(t, res.Err(), actor.ErrLeaseLost)

	// The forward was staged (the child was spawned) ...
	require.Equal(t, 1, rec.spawns)

	// ... but no inline admission completion ran on the failed-commit path:
	// the child was neither awaited-and-dropped nor reaped, and no handoff
	// is left dangling for a later turn to pick up.
	require.Equal(t, 0, rec.stopCount())
	require.Nil(t, b.pendingHandoff)
	require.Len(t, b.active, 1)
}

// TestOORRegistryFailedIncomingForwardDropsFreshChild verifies a failed hint
// forward drops a freshly spawned incoming child while a pre-existing child
// keeps its state.
func TestOORRegistryFailedIncomingForwardDropsFreshChild(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	id := oorSessionID(0x90)
	req := &ResolveIncomingTransferRequest{
		SessionID: id,
		RecipientPkScript: []byte{
			0x51,
			0x20,
			0xcc,
		},
	}

	b, rec := newTestRegistryBehavior(newFakeRegistryStore())

	// Fresh spawn + failed forward: the child is dropped.
	rec.tellErr = errors.New("delivery store down")

	res := b.Receive(ctx, req, fakeExec{})
	require.True(t, res.IsErr())
	require.Empty(t, b.active)
	require.Equal(t, 1, rec.stopCount())

	// Admit the session for real, then fail a duplicate hint's forward:
	// the pre-existing child must survive.
	rec.tellErr = nil
	res = b.Receive(ctx, req, fakeExec{})
	require.True(t, res.IsOk(), res.Err())
	require.Len(t, b.active, 1)

	rec.tellErr = errors.New("delivery store down")
	res = b.Receive(ctx, req, fakeExec{})
	require.True(t, res.IsErr())
	require.Len(t, b.active, 1)
	require.Equal(t, 1, rec.stopCount())
}

// TestOORRegistryChildConfigNormalizesLimits verifies the per-session config
// the registry hands each child carries fully normalized receive limits, so a
// partially-zeroed daemon config cannot disable individual caps.
func TestOORRegistryChildConfigNormalizesLimits(t *testing.T) {
	t.Parallel()

	b, _ := newTestRegistryBehavior(newFakeRegistryStore())
	b.cfg.Limits = ReceiveLimits{MaxCheckpoints: 7}

	cfg := b.childConfig(
		oorSessionID(0xa0), clientdb.OORSessionDirectionIncoming,
	)

	defaults := DefaultReceiveLimits()
	require.Equal(t, uint32(7), cfg.Limits.MaxCheckpoints)
	require.Equal(t, defaults.MaxVTXOMatches, cfg.Limits.MaxVTXOMatches)
	require.Equal(t, defaults.MaxMailboxItems, cfg.Limits.MaxMailboxItems)
	require.Equal(
		t, defaults.MaxMailboxScriptBytes,
		cfg.Limits.MaxMailboxScriptBytes,
	)
}

// TestOORRegistryAsyncAdmissionEndToEnd drives a full outgoing admission
// through the durable registry: the registry's turn only spawns and forwards,
// the child's admission turn (inline signing plus the snapshot commit)
// settles the caller's detached promise, and the control-plane row lands with
// the submit-sent phase.
func TestOORRegistryAsyncAdmissionEndToEnd(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{clientKey}, nil,
	)

	system := actor.NewActorSystem()
	defer func() {
		require.NoError(t, system.Shutdown(context.Background()))
	}()

	store := newFakeRegistryStore()
	registry, err := NewOORRegistryActor(OORRegistryConfig{
		RegistryStore:    store,
		DeliveryStore:    newTestDeliveryStore(t),
		ServerConn:       fakeServerConnRef{},
		Signer:           clientSigner,
		IncomingHandler:  &fakeRecipientFilter{owned: true},
		PackageStore:     &fakePackageStore{},
		ReservationStore: &countingReservationStore{},
		ActorSystem:      system,
	})
	require.NoError(t, err)
	defer registry.Stop()

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, operatorKey.PubKey(), wire.OutPoint{
				Hash:  [32]byte{0x07},
				Index: 0,
			},
			btcutil.Amount(10_000),
		),
	}
	askCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	res := registry.Ref().Ask(askCtx, &StartTransferRequest{
		Policy: arkscript.CheckpointPolicy{
			OperatorKey: operatorKey.PubKey(),
			CSVDelay:    10,
		},
		Inputs: inputs,
		Recipients: []oortx.RecipientOutput{{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputs[0].VTXO.Amount,
		}},
	}).Await(askCtx)
	require.True(t, res.IsOk(), res.Err())

	resp, ok := res.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)
	require.False(t, resp.Existing)
	require.NotEqual(t, SessionID{}, resp.SessionID)

	// The child's admission turn committed the control-plane row with the
	// submit transport collected.
	record, err := store.GetSession(ctx, chainHashOf(resp.SessionID))
	require.NoError(t, err)
	require.Equal(
		t, clientdb.OORSessionDirectionOutgoing, record.Direction,
	)
	require.Equal(t, string(OutgoingPhaseSubmitSent), record.Phase)
}

// TestOORRegistryDetachedAdmissionWaitBounded verifies the
// detached-continuation wait is bounded: a child whose admission future never
// resolves, addressed under an uncancellable caller context
// (context.WithoutCancel, as the production StartTransfer call site derives
// it), must not leak the OnComplete goroutine forever. The registry wraps the
// caller context in detachedWaitTimeout, so the caller's promise completes with
// a deadline error and the continuation goroutine exits.
func TestOORRegistryDetachedAdmissionWaitBounded(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{clientKey}, nil,
	)

	system := actor.NewActorSystem()
	defer func() {
		require.NoError(t, system.Shutdown(context.Background()))
	}()

	store := newFakeRegistryStore()
	registry, err := NewOORRegistryActor(OORRegistryConfig{
		RegistryStore:    store,
		DeliveryStore:    newTestDeliveryStore(t),
		ServerConn:       fakeServerConnRef{},
		Signer:           clientSigner,
		IncomingHandler:  &fakeRecipientFilter{owned: true},
		PackageStore:     &fakePackageStore{},
		ReservationStore: &countingReservationStore{},
		ActorSystem:      system,
	})
	require.NoError(t, err)
	defer registry.Stop()

	// Shrink the bound so the test does not wait the production default,
	// and replace the spawn with a stub child whose admission Ask never
	// resolves, modeling a wedged child turn.
	registry.behavior.detachedWaitTimeout = 200 * time.Millisecond
	registry.behavior.spawnFunc = func(id SessionID,
		dir clientdb.OORSessionDirection) (*OORSessionActor, error) {

		tellRef := &recordingTellRef{
			id:  ActorIDForSession(id),
			rec: &registrySpawnRecorder{},
		}

		return &OORSessionActor{
			ref: &neverResolvingActorRef{
				recordingTellRef: *tellRef,
			},
			tellRef: tellRef,
			stop:    func() {},
		}, nil
	}

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, operatorKey.PubKey(), wire.OutPoint{
				Hash:  [32]byte{0x08},
				Index: 0,
			},
			btcutil.Amount(10_000),
		),
	}

	// The caller's context is uncancellable, exactly as the production
	// StartTransfer call site derives it via context.WithoutCancel; only
	// the registry's detachedWaitTimeout wrap can unblock the continuation.
	askCtx := actor.WithoutTx(context.WithoutCancel(ctx))

	future := registry.Ref().Ask(askCtx, &StartTransferRequest{
		Policy: arkscript.CheckpointPolicy{
			OperatorKey: operatorKey.PubKey(),
			CSVDelay:    10,
		},
		Inputs: inputs,
		Recipients: []oortx.RecipientOutput{{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputs[0].VTXO.Amount,
		}},
	})

	// The continuation must settle the caller's promise off the bound, not
	// off the Await context: a regression that drops the registry's
	// detachedWaitTimeout wrap would leave the promise forever uncompleted
	// (the child future never resolves and the caller context never
	// cancels). Await under an uncancellable context in a goroutine so only
	// a genuine promise completion can unblock it, then assert that
	// completion arrives shortly after the 200ms bound rather than never.
	type awaitResult struct {
		res fn.Result[ActorResp]
	}
	done := make(chan awaitResult, 1)
	go func() {
		done <- awaitResult{res: future.Await(askCtx)}
	}()

	select {
	case got := <-done:
		require.True(t, got.res.IsErr())
		require.ErrorIs(t, got.res.Err(), context.DeadlineExceeded)

	case <-time.After(5 * time.Second):
		t.Fatal(
			"detached admission continuation leaked: caller " +
				"promise never completed",
		)
	}
}

// TestOORRegistryDriveEventRouting verifies the registry's hot path: an
// unknown session errors, a durable non-terminal row restores the child and
// forwards the event via Tell, a terminal row absorbs a late duplicate as a
// no-op, and a failed forward surfaces to the caller.
func TestOORRegistryDriveEventRouting(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sid := oorSessionID(0x73)
	drive := &DriveEventRequest{
		SessionID: sid,
		Event:     &FinalizeAcceptedEvent{},
	}

	// Unknown session with no durable row errors without spawning.
	b, rec := newTestRegistryBehavior(newFakeRegistryStore())
	res := b.handleDriveEvent(ctx, drive)
	require.True(t, res.IsErr())
	require.ErrorContains(t, res.Err(), "unknown session")
	require.Equal(t, 0, rec.spawns)

	// A durable non-terminal row restores the child and forwards the
	// event.
	store := newFakeRegistryStore()
	upsertRegistryRow(
		t, store, sid, clientdb.OORSessionDirectionOutgoing,
		"finalize_sent", "", clientdb.OORSessionStatusPending,
	)
	b, rec = newTestRegistryBehavior(store)

	res = b.handleDriveEvent(ctx, drive)
	require.True(t, res.IsOk(), res.Err())
	require.Equal(t, 1, rec.spawns)

	tells := rec.recorded()
	require.Len(t, tells, 1)
	require.Same(t, drive, tells[0])

	// A terminal row is not restored, but a late duplicate for it is a
	// benign idempotent no-op (acks cleanly) rather than an error, so a
	// normal at-least-once redelivery does not Nack into a dead-letter.
	terminalStore := newFakeRegistryStore()
	upsertRegistryRow(
		t, terminalStore, sid, clientdb.OORSessionDirectionOutgoing,
		"completed", "", clientdb.OORSessionStatusCompleted,
	)
	b, rec = newTestRegistryBehavior(terminalStore)

	res = b.handleDriveEvent(ctx, drive)
	require.True(t, res.IsOk(), res.Err())
	require.Equal(t, 0, rec.spawns)

	// A failed forward propagates the Tell error.
	b, rec = newTestRegistryBehavior(store)
	rec.tellErr = errFilterBroken

	res = b.handleDriveEvent(ctx, drive)
	require.True(t, res.IsErr())
	require.ErrorIs(t, res.Err(), errFilterBroken)
}

// TestOORRegistryStartTransferNoKeyActiveDedup verifies the no-idempotency-key
// dedup path: a StartTransfer whose deterministic session id is already active
// AND backed by a durable row returns Existing without spawning a second child.
func TestOORRegistryStartTransferNoKeyActiveDedup(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, operatorKey.PubKey(), wire.OutPoint{
				Hash:  [32]byte{0x76},
				Index: 0,
			},
			btcutil.Amount(10_000),
		),
	}
	req := &StartTransferRequest{
		Policy: arkscript.CheckpointPolicy{
			OperatorKey: operatorKey.PubKey(),
			CSVDelay:    10,
		},
		Inputs: inputs,
		Recipients: []oortx.RecipientOutput{{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputs[0].VTXO.Amount,
		}},
	}

	// Learn the deterministic session id the same way admission does.
	session, _, err := NewSessionWithIdempotencyKey(
		ctx, req.Policy, req.Inputs, req.Recipients, "",
	)
	require.NoError(t, err)

	store := newFakeRegistryStore()
	b, rec := newTestRegistryBehavior(store)
	b.active[session.ID] = &OORSessionActor{stop: func() {}}

	// A resident child only dedups as Existing when a durable row backs it.
	upsertRegistryRow(
		t, store, session.ID, clientdb.OORSessionDirectionOutgoing,
		"submit_sent", "", clientdb.OORSessionStatusPending,
	)

	res := b.handleStartTransfer(ctx, req)
	require.True(t, res.IsOk(), res.Err())

	resp, ok := res.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)
	require.True(t, resp.Existing)
	require.Equal(t, session.ID, resp.SessionID)
	require.Equal(t, 0, rec.spawns)
}

// TestOORRegistryStartTransferDropsPhantomResident verifies the phantom-child
// race fix: a resident child with NO durable row (a failed admission reaped
// asynchronously by a not-yet-processed SessionTerminalNotification) must not
// answer Existing. The registry drops the phantom synchronously and re-admits a
// fresh child whose request is forwarded, so the retry can make progress
// instead of wedging on an unknown-session DriveEvent later.
func TestOORRegistryStartTransferDropsPhantomResident(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, operatorKey.PubKey(), wire.OutPoint{
				Hash:  [32]byte{0x79},
				Index: 0,
			},
			btcutil.Amount(10_000),
		),
	}
	req := &StartTransferRequest{
		Policy: arkscript.CheckpointPolicy{
			OperatorKey: operatorKey.PubKey(),
			CSVDelay:    10,
		},
		Inputs: inputs,
		Recipients: []oortx.RecipientOutput{{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputs[0].VTXO.Amount,
		}},
	}

	session, _, err := NewSessionWithIdempotencyKey(
		ctx, req.Policy, req.Inputs, req.Recipients, "",
	)
	require.NoError(t, err)

	// Install a resident child with NO durable row: the phantom left behind
	// by a failed admission whose async reap has not run yet.
	store := newFakeRegistryStore()
	b, rec := newTestRegistryBehavior(store)
	var phantomStops int
	b.active[session.ID] = &OORSessionActor{stop: func() {
		phantomStops++
	}}

	res := b.handleStartTransfer(ctx, req)
	require.True(t, res.IsOk(), res.Err())

	resp, ok := res.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)

	// The phantom is not echoed as Existing: it is dropped and a fresh
	// child admits, signing the forwarded request.
	require.False(t, resp.Existing)
	require.Equal(t, session.ID, resp.SessionID)
	require.Equal(t, 1, phantomStops)
	require.Equal(t, 1, rec.spawns)
}

// TestOORRegistryConcurrentTraffic exercises the live registry under
// concurrent admissions and read probes: two distinct transfers admit in
// parallel with list queries interleaved, every turn lands, and the race
// detector sees the whole stack.
func TestOORRegistryConcurrentTraffic(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{clientKey}, nil,
	)

	system := actor.NewActorSystem()
	defer func() {
		require.NoError(t, system.Shutdown(context.Background()))
	}()

	store := newFakeRegistryStore()
	registry, err := NewOORRegistryActor(OORRegistryConfig{
		RegistryStore:    store,
		DeliveryStore:    newTestDeliveryStore(t),
		ServerConn:       fakeServerConnRef{},
		Signer:           clientSigner,
		IncomingHandler:  &fakeRecipientFilter{owned: true},
		PackageStore:     &fakePackageStore{},
		ReservationStore: &countingReservationStore{},
		ActorSystem:      system,
	})
	require.NoError(t, err)
	defer registry.Stop()

	makeRequest := func(seed byte) *StartTransferRequest {
		inputs := []TransferInput{
			newTestTransferInput(
				t, clientKey, operatorKey.PubKey(),
				wire.OutPoint{
					Hash:  [32]byte{seed},
					Index: 0,
				},
				btcutil.Amount(10_000),
			),
		}

		return &StartTransferRequest{
			Policy: arkscript.CheckpointPolicy{
				OperatorKey: operatorKey.PubKey(),
				CSVDelay:    10,
			},
			Inputs: inputs,
			Recipients: []oortx.RecipientOutput{{
				PkScript: newTestTaprootPkScript(
					t, clientKey.PubKey(),
				),
				Value: inputs[0].VTXO.Amount,
			}},
		}
	}

	askCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		admitted []SessionID
		failures []error
	)

	for _, seed := range []byte{0x77, 0x78} {
		wg.Add(1)
		go func(seed byte) {
			defer wg.Done()

			res := registry.Ref().Ask(
				askCtx, makeRequest(seed),
			).Await(askCtx)

			mu.Lock()
			defer mu.Unlock()

			if res.IsErr() {
				failures = append(failures, res.Err())

				return
			}

			resp, ok := res.UnwrapOr(nil).(*StartTransferResponse)
			if !ok || resp.Existing {
				failures = append(
					failures,
					fmt.Errorf(
						"unexpected admission "+
							"response: %v",
						res.UnwrapOr(nil),
					),
				)

				return
			}

			admitted = append(admitted, resp.SessionID)
		}(seed)
	}

	// Interleave read probes with the admissions.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			res := registry.Ref().Ask(
				askCtx, &ListSessionsRequest{},
			).Await(askCtx)

			mu.Lock()
			defer mu.Unlock()

			if res.IsErr() {
				failures = append(failures, res.Err())
			}
		}()
	}

	wg.Wait()

	require.Empty(t, failures)
	require.Len(t, admitted, 2)
	require.NotEqual(t, admitted[0], admitted[1])

	// Both admissions committed durable control-plane rows.
	res := registry.Ref().Ask(askCtx, &ListSessionsRequest{}).Await(askCtx)
	require.True(t, res.IsOk(), res.Err())

	listResp, ok := res.UnwrapOr(nil).(*ListSessionsResponse)
	require.True(t, ok)
	require.Len(t, listResp.Sessions, 2)
}

// TestOORRegistryStopDuringInFlightAdmission verifies Stop() drains the
// registry goroutine before reaping children, so a backlog turn mutating the
// (unsynchronized) active map cannot race stopChildren's iteration. The
// admission traffic is deliberately NOT drained before Stop() -- a stream of
// incoming hints is still being routed (spawning children, writing the active
// map on the registry goroutine) when Stop() fires. Run under -race: without
// the StopAndWait drain this trips "concurrent map iteration and map write".
func TestOORRegistryStopDuringInFlightAdmission(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	system := actor.NewActorSystem()
	defer func() {
		require.NoError(t, system.Shutdown(context.Background()))
	}()

	registry, err := NewOORRegistryActor(OORRegistryConfig{
		RegistryStore:    newFakeRegistryStore(),
		DeliveryStore:    newTestDeliveryStore(t),
		ServerConn:       fakeServerConnRef{},
		Signer:           testSigner(t),
		IncomingHandler:  &fakeRecipientFilter{owned: true},
		PackageStore:     &fakePackageStore{},
		ReservationStore: &countingReservationStore{},
		ActorSystem:      system,
	})
	require.NoError(t, err)

	// Keep a stream of distinct incoming hints flowing into the registry's
	// durable inbox. Each admission spawns a child and writes the active
	// map on the registry goroutine; the sender runs concurrently with
	// Stop() so a backlog turn is still mutating the map when stopChildren
	// iterates it.
	stopSending := make(chan struct{})
	var sender sync.WaitGroup
	sender.Add(1)
	go func() {
		defer sender.Done()

		for i := 0; ; i++ {
			select {
			case <-stopSending:
				return

			default:
			}

			// A cancelled registry context makes Tell fail once
			// Stop fires; that is expected and ends the stream.
			err := registry.Ref().Tell(
				ctx, &ResolveIncomingTransferRequest{
					SessionID: oorSessionID(byte(i)),
					RecipientPkScript: []byte{
						0x51, 0x20, byte(i >> 8), byte(
							i,
						),
					},
					RecipientEventID: uint64(i),
				},
			)
			if err != nil {
				return
			}
		}
	}()

	// Let the backlog build so children are actively being spawned, then
	// Stop while the registry goroutine is still draining. The drain must
	// establish happens-before so stopChildren iterates the active map only
	// after the registry goroutine has exited.
	time.Sleep(20 * time.Millisecond)
	registry.Stop()

	close(stopSending)
	sender.Wait()
}

// TestNewOORRegistryActorValidatesRequiredDeps verifies the registry
// constructor fails loudly on a missing required dependency rather than
// admitting a session and failing mid-transfer (or silently disabling a safety
// net), and that it rejects a SpendCompleter wired without a VTXOStore.
func TestNewOORRegistryActorValidatesRequiredDeps(t *testing.T) {
	t.Parallel()

	// Share one delivery store across every subtest. This test only
	// exercises constructor dependency validation, so a single real store
	// over the test backend is sufficient. Building one per subtest stands
	// up a fresh Postgres fixture (and connection pool) for each case under
	// the test_postgres tag, which exhausts the backend's connections and
	// hangs the run until it times out.
	deliveryStore := newTestDeliveryStore(t)

	valid := func() OORRegistryConfig {
		return OORRegistryConfig{
			RegistryStore: newFakeRegistryStore(),
			DeliveryStore: deliveryStore,
			ServerConn:    fakeServerConnRef{},
			Signer:        testSigner(t),
			IncomingHandler: &fakeRecipientFilter{
				owned: true,
			},
			PackageStore:     &fakePackageStore{},
			ReservationStore: &countingReservationStore{},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*OORRegistryConfig)
		wantErr string
	}{
		{
			name: "missing registry store",
			mutate: func(c *OORRegistryConfig) {
				c.RegistryStore = nil
			},
			wantErr: "registry store",
		},
		{
			name: "missing delivery store",
			mutate: func(c *OORRegistryConfig) {
				c.DeliveryStore = nil
			},
			wantErr: "delivery store",
		},
		{
			name: "missing serverconn",
			mutate: func(c *OORRegistryConfig) {
				c.ServerConn = nil
			},
			wantErr: "serverconn",
		},
		{
			name: "missing signer",
			mutate: func(c *OORRegistryConfig) {
				c.Signer = nil
			},
			wantErr: "signer",
		},
		{
			name: "missing incoming handler",
			mutate: func(c *OORRegistryConfig) {
				c.IncomingHandler = nil
			},
			wantErr: "incoming handler",
		},
		{
			name: "missing package store",
			mutate: func(c *OORRegistryConfig) {
				c.PackageStore = nil
			},
			wantErr: "package store",
		},
		{
			name: "missing reservation store",
			mutate: func(c *OORRegistryConfig) {
				c.ReservationStore = nil
			},
			wantErr: "reservation store",
		},
		{
			name: "spend completer without vtxo store",
			mutate: func(c *OORRegistryConfig) {
				c.SpendCompleter = func(context.Context,
					[]wire.OutPoint) error {

					return nil
				}
			},
			wantErr: "vtxo store",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(&cfg)

			_, err := NewOORRegistryActor(cfg)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
