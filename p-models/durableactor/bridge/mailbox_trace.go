package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db/actordelivery"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	_ "modernc.org/sqlite"
)

// commitActorID and commitDedupTTL are the dedup-mark identity and retention a
// commit trace op writes via MarkProcessed inside the fenced commit
// transaction. They live at package scope so any future trace op that also
// records dedup state shares the same actor id and TTL as commit.
const (
	commitActorID  = "model.TraceActor"
	commitDedupTTL = time.Hour
)

// MailboxTrace is a model trace that can be replayed against the real durable
// actor delivery store. These traces mirror the P model scenarios in
// mailbox_fifo_test.p and keep the formal abstraction tied to the SQL claim
// implementation.
type MailboxTrace struct {
	TraceID     string              `json:"trace_id"`
	Description string              `json:"description"`
	Events      []MailboxTraceEvent `json:"events"`
}

// MailboxTraceEvent describes one store operation in a mailbox trace.
type MailboxTraceEvent struct {
	Op             string `json:"op"`
	ID             string `json:"id,omitempty"`
	MailboxID      string `json:"mailbox_id,omitempty"`
	CorrelationKey string `json:"correlation_key,omitempty"`
	LeaseToken     string `json:"lease_token,omitempty"`
	ExpectID       string `json:"expect_id,omitempty"`
	Payload        string `json:"payload,omitempty"`
	ExpectPayload  string `json:"expect_payload,omitempty"`
	FailureReason  string `json:"failure_reason,omitempty"`
	Now            *int64 `json:"now,omitempty"`
	AvailableAt    int64  `json:"available_at,omitempty"`
	RetryAfter     int64  `json:"retry_after,omitempty"`
	LeaseDuration  int64  `json:"lease_duration,omitempty"`
	MaxAttempts    int    `json:"max_attempts,omitempty"`
	Priority       int    `json:"priority,omitempty"`
	ExpectRows     *int64 `json:"expect_rows,omitempty"`

	// ExpectDuplicate marks an enqueue op whose id already exists.
	// Production EnqueueMessage is idempotent by durable id, so a duplicate
	// must be a no-op that returns no error. Setting this makes the trace
	// assert that intent at the enqueue step itself, rather than relying
	// solely on a downstream lease assertion to observe the no-op.
	ExpectDuplicate bool `json:"expect_duplicate,omitempty"`

	// ExpectProcessed asserts the dedup state of the row after a commit op.
	// A fenced commit that consumes the row marks it processed; a stale
	// (ErrLeaseLost) commit rolls the dedup mark back, leaving it
	// unprocessed. Nil leaves the dedup state unchecked.
	ExpectProcessed *bool `json:"expect_processed,omitempty"`
}

// ParseMailboxTrace parses one mailbox model trace from disk.
func ParseMailboxTrace(path string) (*MailboxTrace, error) {
	//nolint:gosec // Reads checked-in bridge trace files only.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mailbox trace: %w", err)
	}

	var trace MailboxTrace
	if err := json.Unmarshal(data, &trace); err != nil {
		return nil, fmt.Errorf("parse mailbox trace: %w", err)
	}

	if trace.TraceID == "" {
		return nil, fmt.Errorf("mailbox trace missing trace_id")
	}

	return &trace, nil
}

// ParseMailboxTraceDir parses all mailbox model traces from dir.
func ParseMailboxTraceDir(dir string) ([]*MailboxTrace, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read mailbox trace dir: %w", err)
	}

	traces := make([]*MailboxTrace, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		trace, err := ParseMailboxTrace(
			filepath.Join(
				dir, entry.Name(),
			),
		)
		if err != nil {
			return nil, err
		}

		traces = append(traces, trace)
	}

	sort.Slice(traces, func(i, j int) bool {
		return traces[i].TraceID < traces[j].TraceID
	})

	return traces, nil
}

// ReplayMailboxTrace replays a mailbox trace against the production SQLite
// actor-delivery store.
func ReplayMailboxTrace(t *testing.T, trace *MailboxTrace) {
	t.Helper()

	rawDB := newSQLiteDB(t)
	requireNoError(
		t, actordelivery.RunMigrations(
			rawDB, sqlc.BackendTypeSqlite,
		),
	)

	clk := clock.NewTestClock(traceTime(0))
	store, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		rawDB, sqlc.BackendTypeSqlite, clk, btclog.Disabled,
	)
	requireNoError(t, err)

	for i, event := range trace.Events {
		if event.Now != nil {
			clk.SetTime(traceTime(*event.Now))
		}

		switch event.Op {
		case "enqueue":
			replayEnqueue(t, store, event)

		case "lease":
			replayLease(t, store, event)

		case "nack":
			replayNack(t, store, event)

		case "ack":
			replayAck(t, store, event)

		case "commit":
			replayCommit(t, store, event)

		case "dead_letter":
			replayDeadLetter(t, store, event)

		case "expire_leases":
			replayExpireLeases(t, store)

		default:
			t.Fatalf("trace %s event %d: unknown op %q",
				trace.TraceID, i, event.Op)
		}
	}
}

func replayEnqueue(t *testing.T, store actor.TxAwareDeliveryStore,
	event MailboxTraceEvent) {

	t.Helper()

	maxAttempts := event.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 3
	}

	payload := event.Payload
	if payload == "" {
		payload = event.ID
	}

	err := store.EnqueueMessage(
		t.Context(), actor.EnqueueParams{
			ID:             event.ID,
			MailboxID:      event.MailboxID,
			MessageType:    "model.TraceMsg",
			Payload:        []byte(payload),
			Priority:       event.Priority,
			AvailableAt:    traceTime(event.AvailableAt),
			MaxAttempts:    maxAttempts,
			CorrelationKey: event.CorrelationKey,
		},
	)

	// A duplicate enqueue must be a silent no-op rather than an error, so a
	// future change that made EnqueueMessage reject duplicates fails here
	// with a duplicate-specific message instead of at some later lease
	// step.
	if event.ExpectDuplicate {
		if err != nil {
			t.Fatalf("duplicate enqueue of %s expected to be an "+
				"idempotent no-op, got error: %v", event.ID,
				err)
		}

		return
	}

	requireNoError(t, err)
}

func replayLease(t *testing.T, store actor.TxAwareDeliveryStore,
	event MailboxTraceEvent) {

	t.Helper()

	leaseDuration := time.Minute
	if event.LeaseDuration != 0 {
		leaseDuration = time.Duration(event.LeaseDuration) * time.Second
	}

	leased, err := store.LeaseNextMessage(
		t.Context(), event.MailboxID, event.LeaseToken, leaseDuration,
	)
	requireNoError(t, err)

	if event.ExpectID == "" {
		if leased != nil {
			t.Fatalf("expected no leased row, got %s", leased.ID)
		}

		return
	}

	if leased == nil {
		t.Fatalf("expected leased row %s, got nil", event.ExpectID)
	}

	if leased.ID != event.ExpectID {
		t.Fatalf("expected leased row %s, got %s", event.ExpectID,
			leased.ID)
	}

	if event.ExpectPayload != "" &&
		string(leased.Payload) != event.ExpectPayload {

		t.Fatalf("expected leased payload %q, got %q",
			event.ExpectPayload, string(leased.Payload))
	}
}

func replayNack(t *testing.T, store actor.TxAwareDeliveryStore,
	event MailboxTraceEvent) {

	t.Helper()

	rows, err := store.NackMessage(
		t.Context(), event.ID, event.LeaseToken,
		time.Duration(event.RetryAfter)*time.Second,
	)
	requireNoError(t, err)
	requireExpectedRows(t, rows, event, "nack")
}

func replayAck(t *testing.T, store actor.TxAwareDeliveryStore,
	event MailboxTraceEvent) {

	t.Helper()

	rows, err := store.AckMessage(
		t.Context(), event.ID, event.LeaseToken,
	)
	requireNoError(t, err)
	requireExpectedRows(t, rows, event, "ack")
}

// replayCommit models the durable actor's Read/Commit consume step against the
// real store, mirroring execCore.commit in baselib/actor: inside one writer
// transaction it fence-acks the row (DELETE ... WHERE id AND lease_token) and,
// only when that ack consumed a row, records the dedup mark. A zero-row ack
// means the lease was reclaimed mid-IO, so the transaction is rolled back with
// ErrLeaseLost and nothing (ack or dedup mark) is applied. This keeps the P
// commit-fence model tied to the SQL exactly-once mechanism.
func replayCommit(t *testing.T, store actor.TxAwareDeliveryStore,
	event MailboxTraceEvent) {

	t.Helper()

	var rows int64
	err := store.ExecTx(
		t.Context(), false,
		func(txCtx context.Context, s actor.DeliveryStore) error {
			var ackErr error
			rows, ackErr = s.AckMessage(
				txCtx, event.ID, event.LeaseToken,
			)
			if ackErr != nil {
				return ackErr
			}

			// Fence: a zero-row ack means the lease was lost. Abort
			// so the dedup mark (and, in production, the behavior's
			// domain writes) roll back atomically with the failed
			// consume.
			if rows == 0 {
				return actor.ErrLeaseLost
			}

			return s.MarkProcessed(
				txCtx, event.ID, commitActorID, commitDedupTTL,
			)
		},
	)

	// A fenced commit either consumes the row (rows == 1, no error) or is a
	// lease-lost no-op (rows == 0, ErrLeaseLost, fully rolled back).
	if rows == 0 {
		if !errors.Is(err, actor.ErrLeaseLost) {
			t.Fatalf("commit of %s with a stale lease expected "+
				"ErrLeaseLost, got %v", event.ID, err)
		}
	} else {
		requireNoError(t, err)
	}

	requireExpectedRows(t, rows, event, "commit")

	// The dedup mark is the bridge's analog of the behavior effect: it is
	// folded into the same transaction as the ack, so a stale commit leaves
	// the row unprocessed while a successful commit marks it exactly once.
	if event.ExpectProcessed != nil {
		processed, perr := store.IsProcessed(t.Context(), event.ID)
		requireNoError(t, perr)
		if processed != *event.ExpectProcessed {
			t.Fatalf("commit of %s: expected processed=%v, got %v",
				event.ID, *event.ExpectProcessed, processed)
		}
	}
}

func replayDeadLetter(t *testing.T, store actor.TxAwareDeliveryStore,
	event MailboxTraceEvent) {

	t.Helper()

	reason := event.FailureReason
	if reason == "" {
		reason = "model trace dead letter"
	}

	rows, err := store.MoveToDeadLetter(
		t.Context(), event.ID, event.LeaseToken, reason,
	)
	requireNoError(t, err)
	requireExpectedRows(t, rows, event, "dead_letter")

	deadLetter, err := store.GetDeadLetter(t.Context(), "mailbox", event.ID)
	requireNoError(t, err)
	if deadLetter == nil {
		t.Fatalf("expected dead letter %s, got nil", event.ID)
	}

	if deadLetter.FailureReason != reason {
		t.Fatalf("expected dead letter reason %q, got %q", reason,
			deadLetter.FailureReason)
	}
}

func replayExpireLeases(t *testing.T, store actor.TxAwareDeliveryStore) {
	t.Helper()

	requireNoError(t, store.ExpireLeases(t.Context()))
}

func newSQLiteDB(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "mailbox-bridge.db")
	rawDB, err := sql.Open("sqlite", path)
	requireNoError(t, err)

	t.Cleanup(func() {
		requireNoError(t, rawDB.Close())
	})

	return rawDB
}

func traceTime(seconds int64) time.Time {
	return time.Unix(1_700_000_000+seconds, 0)
}

func requireNoError(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}

func requireExpectedRows(t *testing.T, rows int64, event MailboxTraceEvent,
	op string) {

	t.Helper()

	expected := int64(1)
	if event.ExpectRows != nil {
		expected = *event.ExpectRows
	}

	if rows != expected {
		t.Fatalf("%s affected %d rows, expected %d", op, rows, expected)
	}
}
