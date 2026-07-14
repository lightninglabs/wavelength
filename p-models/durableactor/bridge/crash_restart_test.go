package bridge

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db/actordelivery"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

// openStoreAt opens (or reopens) an actor-delivery store backed by the SQLite
// file at path. runMigrations applies the schema, which the first open must do
// and a restart open must not (the schema is already durable in the file). The
// returned closer drops the underlying *sql.DB connection, modeling a process
// exit that leaves the on-disk database intact.
func openStoreAt(t *testing.T, path string, clk clock.Clock,
	runMigrations bool) (actor.TxAwareDeliveryStore, func()) {

	t.Helper()

	rawDB, err := sql.Open("sqlite", path)
	requireNoError(t, err)

	if runMigrations {
		requireNoError(
			t, actordelivery.RunMigrations(
				rawDB, sqlc.BackendTypeSqlite,
			),
		)
	}

	store, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		rawDB, sqlc.BackendTypeSqlite, clk, btclog.Disabled,
	)
	requireNoError(t, err)

	return store, func() {
		requireNoError(t, rawDB.Close())
	}
}

// TestPeekSurvivesCrashBeforeAck models a single-worker actor that peeks a
// durable message (the leaseless fast path), then the process dies before it
// can ack or nack. After a restart against the same on-disk database the same
// row must still be peekable, byte-for-byte, with its attempt count unchanged:
// peek is read-only, so a crash mid-processing cannot lose the message or
// inflate its retry budget. The restart is real — a fresh *sql.DB and a fresh
// store are constructed against the same file — so this exercises durability,
// not in-memory state.
func TestPeekSurvivesCrashBeforeAck(t *testing.T) {
	t.Parallel()

	const (
		msgID   = "crash-peek-1"
		mailbox = "single-worker"
	)

	path := filepath.Join(t.TempDir(), "crash-restart.db")
	clk := clock.NewTestClock(traceTime(0))

	// First boot: enqueue a message and peek it without acking.
	store, closeStore := openStoreAt(t, path, clk, true)

	requireNoError(
		t,
		store.EnqueueMessage(
			t.Context(), actor.EnqueueParams{
				ID:          msgID,
				MailboxID:   mailbox,
				MessageType: "model.TraceMsg",
				Payload:     []byte("crash-payload"),
				AvailableAt: clk.Now(),
				MaxAttempts: 3,
			},
		),
	)

	peeked, err := store.PeekNextMessage(t.Context(), mailbox)
	requireNoError(t, err)
	if peeked == nil || peeked.ID != msgID {
		t.Fatalf("expected to peek %s before crash, got %v", msgID,
			peeked)
	}
	if peeked.LeaseToken != "" {
		t.Fatalf("leaseless peek must surface an empty token, got %q",
			peeked.LeaseToken)
	}
	if peeked.Attempts != 0 {
		t.Fatalf("peek must not increment attempts, got %d",
			peeked.Attempts)
	}

	// Crash: drop the connection with the message peeked but neither acked
	// nor nacked.
	closeStore()

	// Restart: a fresh store against the same on-disk database.
	clk2 := clock.NewTestClock(traceTime(5))
	store2, closeStore2 := openStoreAt(t, path, clk2, false)
	defer closeStore2()

	// The same row is still peekable, intact, with attempts unchanged: the
	// pre-crash peek neither consumed it nor charged it a retry.
	rePeeked, err := store2.PeekNextMessage(t.Context(), mailbox)
	requireNoError(t, err)
	if rePeeked == nil || rePeeked.ID != msgID {
		t.Fatalf("expected %s to survive the restart, got %v", msgID,
			rePeeked)
	}
	if rePeeked.LeaseToken != "" {
		t.Fatalf("post-restart peek must still be leaseless, got %q",
			rePeeked.LeaseToken)
	}
	if rePeeked.Attempts != 0 {
		t.Fatalf("attempts must be unchanged across the crash, got %d",
			rePeeked.Attempts)
	}
	if string(rePeeked.Payload) != "crash-payload" {
		t.Fatalf("payload corrupted across restart: %q",
			string(rePeeked.Payload))
	}

	// The recovered worker can now consume it via the leaseless by-id ack.
	rows, err := store2.AckMessageByID(t.Context(), msgID)
	requireNoError(t, err)
	if rows != 1 {
		t.Fatalf("expected to ack exactly 1 recovered row, got %d",
			rows)
	}
	if drained, err := store2.PeekNextMessage(
		t.Context(), mailbox,
	); err != nil || drained != nil {

		t.Fatalf("expected the mailbox to be drained after ack, got "+
			"(%v, %v)", drained, err)
	}
}

// TestLeaseSurvivesCrashBeforeAck is the leased-path counterpart: a worker
// leases a message (which sets a lease token and increments attempts), then the
// process dies mid-IO before acking. After a restart the row must still exist
// with its incremented attempt count durable; once the original lease deadline
// passes, ExpireLeases returns it to the available set and it can be re-leased
// for at-least-once redelivery — the durable-redelivery contract under a crash.
func TestLeaseSurvivesCrashBeforeAck(t *testing.T) {
	t.Parallel()

	const (
		msgID   = "crash-lease-1"
		mailbox = "multi-worker"
	)

	path := filepath.Join(t.TempDir(), "crash-lease.db")
	clk := clock.NewTestClock(traceTime(0))

	// First boot: enqueue and lease (token + attempts++), then crash.
	store, closeStore := openStoreAt(t, path, clk, true)

	requireNoError(
		t,
		store.EnqueueMessage(
			t.Context(), actor.EnqueueParams{
				ID:          msgID,
				MailboxID:   mailbox,
				MessageType: "model.TraceMsg",
				Payload:     []byte("lease-payload"),
				AvailableAt: clk.Now(),
				MaxAttempts: 3,
			},
		),
	)

	leased, err := store.LeaseNextMessage(
		t.Context(), mailbox, "worker-A", 10*time.Second,
	)
	requireNoError(t, err)
	if leased == nil || leased.ID != msgID {
		t.Fatalf("expected to lease %s before crash, got %v", msgID,
			leased)
	}
	if leased.Attempts != 1 {
		t.Fatalf("lease must increment attempts to 1, got %d",
			leased.Attempts)
	}

	// Crash mid-IO: the lease is held but never acked.
	closeStore()

	// Restart at a time still within the original lease window: the row is
	// durably present, the attempt bump survived, and the stale lease still
	// fences a peek out until it expires.
	clk2 := clock.NewTestClock(traceTime(5))
	store2, closeStore2 := openStoreAt(t, path, clk2, false)
	defer closeStore2()

	if peeked, err := store2.PeekNextMessage(
		t.Context(), mailbox,
	); err != nil || peeked != nil {

		t.Fatalf("a still-live lease must fence the row out "+
			"post-restart, got (%v, %v)", peeked, err)
	}

	// Once the lease deadline passes, expiry returns the row to the
	// available set and a new worker re-leases it — the attempt count
	// carries forward, so dead-lettering accounting is preserved.
	clk2.SetTime(traceTime(20))
	requireNoError(t, store2.ExpireLeases(t.Context()))

	released, err := store2.LeaseNextMessage(
		t.Context(), mailbox, "worker-B", 10*time.Second,
	)
	requireNoError(t, err)
	if released == nil || released.ID != msgID {
		t.Fatalf("expected %s to be re-leasable after expiry, got %v",
			msgID, released)
	}
	if released.Attempts != 2 {
		t.Fatalf("expected attempts to carry forward to 2 across the "+
			"crash, got %d", released.Attempts)
	}
	if string(released.Payload) != "lease-payload" {
		t.Fatalf("payload corrupted across restart: %q",
			string(released.Payload))
	}
}
