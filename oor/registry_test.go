package oor

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	clientdb "github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
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
		},
		log:    btclog.Disabled,
		active: make(map[SessionID]*OORSessionActor),
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
			})
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
		res := b.Receive(ctx, &ResumeSessionRequest{SessionID: id})
		require.True(t, res.IsOk(), res.Err())
	}

	require.Equal(t, 1, rec.spawns)
	require.Len(t, rec.recorded(), 2)
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
			})

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
