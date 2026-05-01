package db

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo/mailbox"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

// newTestMailboxStore creates a MailboxEnvelopeStore backed by a
// test SQLite database with the default configuration.
func newTestMailboxStore(t testing.TB,
	opts ...mailbox.StoreOption) *MailboxEnvelopeStore {

	t.Helper()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)

	return NewMailboxEnvelopeStore(store, btclog.Disabled, opts...)
}

// makeTestEnvelope creates a test envelope with the given recipient
// and msg_id.
func makeTestEnvelope(recipient, msgID string) *mailbox.Envelope {
	return &mailboxpb.Envelope{
		Recipient:       recipient,
		MsgId:           msgID,
		Sender:          "test-sender",
		ProtocolVersion: 1,
		CreatedAtUnixMs: time.Now().UnixMilli(),
		Body: &anypb.Any{
			TypeUrl: "type.googleapis.com/test.TestMsg",
			Value:   []byte("payload-" + msgID),
		},
	}
}

// TestMailboxStoreAppendPullRoundTrip verifies that envelopes can be
// appended and pulled back with correct sequence assignment.
func TestMailboxStoreAppendPullRoundTrip(t *testing.T) {
	t.Parallel()

	store := newTestMailboxStore(t)
	ctx := t.Context()

	// Append two envelopes.
	seq1, err := store.Append(ctx, makeTestEnvelope("alice", "msg-1"))
	require.NoError(t, err)
	require.Greater(t, seq1, uint64(0))

	seq2, err := store.Append(ctx, makeTestEnvelope("alice", "msg-2"))
	require.NoError(t, err)
	require.Greater(t, seq2, seq1)

	// Pull from cursor 0 should return both.
	envs, nextCursor, err := store.Pull(
		ctx, "alice", 0, 10,
	)
	require.NoError(t, err)
	require.Len(t, envs, 2)
	require.Equal(t, seq2+1, nextCursor)

	// Verify envelope content survives the round-trip.
	require.Equal(t, "msg-1", envs[0].MsgId)
	require.Equal(t, "msg-2", envs[1].MsgId)
	require.Equal(t, "alice", envs[0].Recipient)
}

// TestMailboxStoreAppendIdempotent verifies that appending the same
// msg_id twice is treated as idempotent (no error, no duplicate).
func TestMailboxStoreAppendIdempotent(t *testing.T) {
	t.Parallel()

	store := newTestMailboxStore(t)
	ctx := t.Context()

	env := makeTestEnvelope("alice", "dup-msg")

	seq1, err := store.Append(ctx, env)
	require.NoError(t, err)
	require.Greater(t, seq1, uint64(0))

	// Second append with same msg_id should succeed (idempotent).
	seq2, err := store.Append(ctx, env)
	require.NoError(t, err)

	// Should return 0 (idempotent path) not a duplicate row.
	require.Equal(t, uint64(0), seq2)

	// Should still have only one envelope.
	envs, _, err := store.Pull(ctx, "alice", 0, 10)
	require.NoError(t, err)
	require.Len(t, envs, 1)
}

// TestMailboxStoreAckCursorMonotonic verifies that the ack cursor
// only advances forward and garbage-collects old envelopes.
func TestMailboxStoreAckCursorMonotonic(t *testing.T) {
	t.Parallel()

	store := newTestMailboxStore(t)
	ctx := t.Context()

	// Append 3 envelopes.
	for i := 1; i <= 3; i++ {
		msgID := fmt.Sprintf("msg-%d", i)
		_, err := store.Append(
			ctx, makeTestEnvelope("bob", msgID),
		)
		require.NoError(t, err)
	}

	// Pull all 3.
	envs, _, err := store.Pull(ctx, "bob", 0, 10)
	require.NoError(t, err)
	require.Len(t, envs, 3)

	// Ack up to the second envelope's sequence + 1.
	ackCursor := envs[1].EventSeq + 1
	err = store.AckUpTo(ctx, "bob", ackCursor)
	require.NoError(t, err)

	// Pull from 0 should only return the third envelope (first
	// two were GC'd).
	envs, _, err = store.Pull(ctx, "bob", 0, 10)
	require.NoError(t, err)
	require.Len(t, envs, 1)

	// Attempting to decrease the cursor should be a no-op.
	err = store.AckUpTo(ctx, "bob", 1)
	require.NoError(t, err)

	// Still one envelope remaining.
	envs, _, err = store.Pull(ctx, "bob", 0, 10)
	require.NoError(t, err)
	require.Len(t, envs, 1)
}

// TestMailboxStoreSequenceMonotonicAfterGC asserts that event_seq is
// strictly monotonic across append/ack cycles, even when the mailbox
// goes empty between appends. SQLite's INTEGER PRIMARY KEY without
// AUTOINCREMENT reuses ROWIDs of deleted rows, which would let a
// freshly assigned sequence land at or below a client's persisted ack
// cursor and silently hide the new envelope on the next pull.
func TestMailboxStoreSequenceMonotonicAfterGC(t *testing.T) {
	t.Parallel()

	store := newTestMailboxStore(t)
	ctx := t.Context()

	// Append, pull, ack, GC for the same recipient repeatedly. The
	// returned sequences must never repeat or regress, because clients
	// persist their ack cursor across these cycles and a regressed
	// sequence would make the new envelope invisible to a pull
	// starting from the prior cursor.
	const cycles = 5

	var maxSeq uint64
	for i := 0; i < cycles; i++ {
		msgID := fmt.Sprintf("msg-%d", i)

		seq, err := store.Append(
			ctx, makeTestEnvelope("alice", msgID),
		)
		require.NoError(t, err)

		require.Greater(t, seq, maxSeq,
			"sequence %d regressed below prior max %d on cycle "+
				"%d", seq, maxSeq, i)
		maxSeq = seq

		envs, _, err := store.Pull(ctx, "alice", 0, 10)
		require.NoError(t, err)
		require.Len(t, envs, 1)
		require.Equal(t, seq, envs[0].EventSeq)

		require.NoError(
			t, store.AckUpTo(ctx, "alice", seq+1),
		)
	}

	// Cross-recipient interleaving: append for bob between two alice
	// cycles. Bob's row keeps the table non-empty when alice's row is
	// GC'd, but alice's next sequence still must not collide with bob's
	// or with any prior alice sequence.
	bobSeq, err := store.Append(
		ctx, makeTestEnvelope("bob", "bob-msg"),
	)
	require.NoError(t, err)
	require.Greater(t, bobSeq, maxSeq)
	maxSeq = bobSeq

	aliceSeq, err := store.Append(
		ctx, makeTestEnvelope("alice", "alice-final"),
	)
	require.NoError(t, err)
	require.Greater(t, aliceSeq, maxSeq)
}

// TestMailboxStorePullSeesEnvelopeAppendedAfterGC mirrors the
// production failure mode: a client persists an ack cursor of N, the
// mailbox goes empty under GC, and the operator appends a fresh
// envelope. The new envelope must be visible to a pull starting from
// cursor N.
func TestMailboxStorePullSeesEnvelopeAppendedAfterGC(t *testing.T) {
	t.Parallel()

	store := newTestMailboxStore(t)
	ctx := t.Context()

	first, err := store.Append(
		ctx, makeTestEnvelope("alice", "first"),
	)
	require.NoError(t, err)

	// Client consumes the first envelope and advances its ack cursor.
	clientCursor := first + 1
	require.NoError(
		t, store.AckUpTo(ctx, "alice", clientCursor),
	)

	// Mailbox is now empty. Any new append must produce a sequence
	// >= clientCursor so the client's next pull sees it.
	second, err := store.Append(
		ctx, makeTestEnvelope("alice", "second"),
	)
	require.NoError(t, err)
	require.GreaterOrEqual(t, second, clientCursor,
		"new envelope sequence %d must be >= persisted client "+
			"cursor %d", second, clientCursor)

	envs, _, err := store.Pull(ctx, "alice", clientCursor, 10)
	require.NoError(t, err)
	require.Len(t, envs, 1)
	require.Equal(t, second, envs[0].EventSeq)
}

// TestMailboxStorePullContextCancel verifies that Pull returns when
// the context is canceled while waiting for envelopes.
func TestMailboxStorePullContextCancel(t *testing.T) {
	t.Parallel()

	store := newTestMailboxStore(t,
		mailbox.WithPullPollInterval(10*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(
		t.Context(), 100*time.Millisecond,
	)
	defer cancel()

	// Pull from empty mailbox should block until context expires.
	_, _, err := store.Pull(ctx, "empty", 0, 10)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestMailboxStorePullWakesOnAppend verifies that same-process appends wake a
// waiting Pull without waiting for the SQL polling fallback interval.
func TestMailboxStorePullWakesOnAppend(t *testing.T) {
	t.Parallel()

	store := newTestMailboxStore(t,
		mailbox.WithPullPollInterval(time.Hour),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	type pullResult struct {
		envs       []*mailbox.Envelope
		nextCursor uint64
		err        error
	}

	resultChan := make(chan pullResult, 1)
	start := time.Now()

	go func() {
		envs, nextCursor, err := store.Pull(ctx, "wakeup", 0, 10)
		resultChan <- pullResult{
			envs:       envs,
			nextCursor: nextCursor,
			err:        err,
		}
	}()

	time.Sleep(50 * time.Millisecond)

	seq, err := store.Append(ctx, makeTestEnvelope("wakeup", "msg-1"))
	require.NoError(t, err)

	var result pullResult
	select {
	case result = <-resultChan:

	case <-ctx.Done():
		t.Fatalf("pull did not wake on append: %v", ctx.Err())
	}

	require.NoError(t, result.err)
	require.Len(t, result.envs, 1)
	require.Equal(t, "msg-1", result.envs[0].MsgId)
	require.Equal(t, seq+1, result.nextCursor)
	require.Less(t, time.Since(start), time.Second)
}

// TestMailboxStoreMaxEnvelopeBytes verifies that envelopes exceeding
// the size limit are rejected.
func TestMailboxStoreMaxEnvelopeBytes(t *testing.T) {
	t.Parallel()

	store := newTestMailboxStore(t,
		mailbox.WithMaxEnvelopeBytes(50),
	)
	ctx := t.Context()

	// Create a large envelope.
	env := makeTestEnvelope("alice", "big-msg")
	env.Body.Value = make([]byte, 100)

	_, err := store.Append(ctx, env)
	require.Error(t, err)

	var tooLarge *mailbox.ErrEnvelopeTooLarge
	require.ErrorAs(t, err, &tooLarge)
}

// TestMailboxStoreMaxEnvelopesPerMailbox verifies that mailboxes
// reject new envelopes when at capacity.
func TestMailboxStoreMaxEnvelopesPerMailbox(t *testing.T) {
	t.Parallel()

	store := newTestMailboxStore(t,
		mailbox.WithMaxEnvelopesPerMailbox(2),
	)
	ctx := t.Context()

	// Fill to capacity.
	_, err := store.Append(
		ctx, makeTestEnvelope("alice", "msg-1"),
	)
	require.NoError(t, err)

	_, err = store.Append(
		ctx, makeTestEnvelope("alice", "msg-2"),
	)
	require.NoError(t, err)

	// Third should fail.
	_, err = store.Append(
		ctx, makeTestEnvelope("alice", "msg-3"),
	)
	require.Error(t, err)

	var full *mailbox.ErrMailboxFull
	require.ErrorAs(t, err, &full)
	require.Equal(t, "alice", full.Recipient)

	// Different recipient should still work.
	_, err = store.Append(
		ctx, makeTestEnvelope("bob", "msg-1"),
	)
	require.NoError(t, err)
}

// TestMailboxStoreIsolation verifies that envelopes are isolated
// between recipients.
func TestMailboxStoreIsolation(t *testing.T) {
	t.Parallel()

	store := newTestMailboxStore(t)
	ctx := t.Context()

	_, err := store.Append(
		ctx, makeTestEnvelope("alice", "alice-msg"),
	)
	require.NoError(t, err)

	_, err = store.Append(
		ctx, makeTestEnvelope("bob", "bob-msg"),
	)
	require.NoError(t, err)

	// Alice should only see her envelope.
	envs, _, err := store.Pull(ctx, "alice", 0, 10)
	require.NoError(t, err)
	require.Len(t, envs, 1)
	require.Equal(t, "alice-msg", envs[0].MsgId)

	// Bob should only see his envelope.
	envs, _, err = store.Pull(ctx, "bob", 0, 10)
	require.NoError(t, err)
	require.Len(t, envs, 1)
	require.Equal(t, "bob-msg", envs[0].MsgId)
}

// TestMailboxStoreConcurrentAppendPull verifies that concurrent
// Append and Pull operations do not race or corrupt state.
func TestMailboxStoreConcurrentAppendPull(t *testing.T) {
	t.Parallel()

	store := newTestMailboxStore(t,
		mailbox.WithPullPollInterval(5*time.Millisecond),
	)
	ctx := t.Context()

	const (
		numWriters    = 5
		msgsPerWriter = 10
		recipient     = "concurrent-test"
	)

	// Launch writers that append concurrently.
	var wg sync.WaitGroup
	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()

			for i := 0; i < msgsPerWriter; i++ {
				msgID := fmt.Sprintf(
					"w%d-msg-%d", writerID, i,
				)
				_, err := store.Append(
					ctx,
					makeTestEnvelope(recipient, msgID),
				)
				require.NoError(t, err)
			}
		}(w)
	}

	// Launch a reader that pulls concurrently with writers.
	pullCtx, pullCancel := context.WithTimeout(
		ctx, 2*time.Second,
	)
	defer pullCancel()

	var (
		pulledMu sync.Mutex
		pulled   = make(map[string]struct{})
	)

	wg.Add(1)
	go func() {
		defer wg.Done()

		var cursor uint64
		for {
			envs, next, err := store.Pull(
				pullCtx, recipient, cursor, 50,
			)
			if err != nil {
				// Context expired — done reading.
				return
			}

			pulledMu.Lock()
			for _, env := range envs {
				pulled[env.MsgId] = struct{}{}
			}
			pulledMu.Unlock()

			cursor = next
		}
	}()

	// Wait for all writers to finish, then let the reader
	// drain remaining envelopes.
	wg.Wait()

	// Final pull to collect anything the concurrent reader
	// missed after writers finished.
	envs, _, err := store.Pull(ctx, recipient, 0, 200)
	require.NoError(t, err)

	// All messages from all writers should be present.
	require.Equal(t, numWriters*msgsPerWriter, len(envs))
}

// TestMailboxStoreValidation verifies that invalid inputs are
// rejected.
func TestMailboxStoreValidation(t *testing.T) {
	t.Parallel()

	store := newTestMailboxStore(t)
	ctx := t.Context()

	// Nil envelope.
	_, err := store.Append(ctx, nil)
	require.Error(t, err)

	// Empty recipient.
	_, err = store.Append(ctx, makeTestEnvelope("", "msg"))
	require.Error(t, err)

	// Empty msg_id.
	env := makeTestEnvelope("alice", "")
	env.MsgId = ""
	_, err = store.Append(ctx, env)
	require.Error(t, err)

	// Pull with empty recipient.
	_, _, err = store.Pull(ctx, "", 0, 10)
	require.Error(t, err)

	// Pull with zero limit.
	_, _, err = store.Pull(ctx, "alice", 0, 0)
	require.Error(t, err)

	// AckUpTo with empty recipient.
	err = store.AckUpTo(ctx, "", 1)
	require.Error(t, err)
}
