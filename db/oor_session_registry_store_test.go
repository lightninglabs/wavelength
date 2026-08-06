package db

import (
	"bytes"
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newOORSessionRegistryStoreForTest creates an OOR session registry store
// backed by a fresh test database.
func newOORSessionRegistryStoreForTest(
	t *testing.T) *OORSessionRegistryStoreDB {

	t.Helper()

	db := NewTestDB(t)

	txExec := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	return &OORSessionRegistryStoreDB{
		TransactionExecutor: txExec,
		clock:               clock.NewDefaultClock(),
	}
}

// sessionHash builds a deterministic 32-byte session id from a seed byte.
func sessionHash(seed byte) chainhash.Hash {
	var h chainhash.Hash
	h[0] = seed
	h[1] = 0xab

	return h
}

// TestOORSessionRegistryRoundTrip verifies a full record survives a write and
// read with every field intact, including the opaque snapshot blob.
func TestOORSessionRegistryRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newOORSessionRegistryStoreForTest(t)

	sid := sessionHash(0x01)
	snapshot := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01}

	want := OORSessionRegistryRecord{
		SessionID:       sid,
		ActorID:         "oor-session-" + sid.String(),
		Direction:       OORSessionDirectionOutgoing,
		Phase:           "submit_sent",
		IdempotencyKey:  "idem-key-1",
		Status:          OORSessionStatusPending,
		SnapshotData:    snapshot,
		SnapshotVersion: 1,
		FlowVersion:     1,
	}

	require.NoError(t, store.UpsertSession(ctx, want))

	got, err := store.GetSession(ctx, sid)
	require.NoError(t, err)

	require.Equal(t, want.SessionID, got.SessionID)
	require.Equal(t, want.ActorID, got.ActorID)
	require.Equal(t, want.Direction, got.Direction)
	require.Equal(t, want.Phase, got.Phase)
	require.Equal(t, want.IdempotencyKey, got.IdempotencyKey)
	require.Equal(t, want.Status, got.Status)
	require.True(t, bytes.Equal(want.SnapshotData, got.SnapshotData))
	require.Equal(t, want.SnapshotVersion, got.SnapshotVersion)
	require.Equal(t, want.FlowVersion, got.FlowVersion)
	require.False(t, got.CreatedAt.IsZero())
	require.False(t, got.UpdatedAt.IsZero())
}

// TestOORSessionRegistryGetNotFound verifies a missing session returns the
// sentinel error.
func TestOORSessionRegistryGetNotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newOORSessionRegistryStoreForTest(t)

	_, err := store.GetSession(ctx, sessionHash(0x99))
	require.ErrorIs(t, err, ErrOORSessionNotFound)
}

// TestOORSessionRegistryUpsertUpdates verifies a second upsert advances the
// mutable fields while preserving the original created_at.
func TestOORSessionRegistryUpsertUpdates(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newOORSessionRegistryStoreForTest(t)

	sid := sessionHash(0x02)

	require.NoError(
		t,
		store.UpsertSession(
			ctx, OORSessionRegistryRecord{
				SessionID:       sid,
				ActorID:         "actor-a",
				Direction:       OORSessionDirectionOutgoing,
				Phase:           "ark_sign_requested",
				Status:          OORSessionStatusPending,
				SnapshotData:    []byte{0x01},
				SnapshotVersion: 1,
			},
		),
	)

	first, err := store.GetSession(ctx, sid)
	require.NoError(t, err)

	require.NoError(
		t,
		store.UpsertSession(
			ctx, OORSessionRegistryRecord{
				SessionID:       sid,
				ActorID:         "actor-a",
				Direction:       OORSessionDirectionOutgoing,
				Phase:           "finalize_sent",
				Status:          OORSessionStatusPending,
				SnapshotData:    []byte{0x02, 0x03},
				SnapshotVersion: 1,
				CreatedAt:       first.CreatedAt,
			},
		),
	)

	second, err := store.GetSession(ctx, sid)
	require.NoError(t, err)

	require.Equal(t, "finalize_sent", second.Phase)
	require.True(t, bytes.Equal([]byte{0x02, 0x03}, second.SnapshotData))
	require.Equal(t, first.CreatedAt.Unix(), second.CreatedAt.Unix())
}

// TestOORSessionRegistryListNonTerminal verifies restore queries only return
// non-terminal rows.
func TestOORSessionRegistryListNonTerminal(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newOORSessionRegistryStoreForTest(t)

	pending := sessionHash(0x10)
	completed := sessionHash(0x20)
	failed := sessionHash(0x30)

	upsert := func(sid chainhash.Hash, status OORSessionStatus) {
		rec := OORSessionRegistryRecord{
			SessionID: sid,
			ActorID:   "actor-" + sid.String(),
			Direction: OORSessionDirectionIncoming,
			Phase:     "resolve_pending",
			Status:    status,
			SnapshotData: []byte{
				byte(status),
			},
			SnapshotVersion: 1,
		}
		require.NoError(t, store.UpsertSession(ctx, rec))
	}

	upsert(pending, OORSessionStatusPending)
	upsert(completed, OORSessionStatusCompleted)
	upsert(failed, OORSessionStatusFailed)

	rows, err := store.ListNonTerminal(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, pending, rows[0].SessionID)
}

// TestOORSessionRegistryLookupActiveByIdempotencyKey verifies outgoing
// idempotency-key lookup hits and misses behave correctly, and that failed
// sessions never answer for a key.
func TestOORSessionRegistryLookupActiveByIdempotencyKey(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newOORSessionRegistryStoreForTest(t)

	sid := sessionHash(0x40)
	record := OORSessionRegistryRecord{
		SessionID:      sid,
		ActorID:        "actor-key",
		Direction:      OORSessionDirectionOutgoing,
		Phase:          "submit_sent",
		IdempotencyKey: "the-key",
		Status:         OORSessionStatusPending,
		SnapshotData: []byte{
			0x01,
		},
		SnapshotVersion: 1,
	}
	require.NoError(t, store.UpsertSession(ctx, record))

	got, err := store.LookupActiveSessionByIdempotencyKey(ctx, "the-key")
	require.NoError(t, err)
	require.Equal(t, sid, got.SessionID)

	_, err = store.LookupActiveSessionByIdempotencyKey(ctx, "missing")
	require.ErrorIs(t, err, ErrOORSessionNotFound)

	// An empty key never matches the NULL idempotency rows.
	_, err = store.LookupActiveSessionByIdempotencyKey(ctx, "")
	require.ErrorIs(t, err, ErrOORSessionNotFound)

	// A completed session still dedups its key: replaying a finished
	// transfer must return the existing session, never re-send funds.
	record.Status = OORSessionStatusCompleted
	require.NoError(t, store.UpsertSession(ctx, record))

	got, err = store.LookupActiveSessionByIdempotencyKey(ctx, "the-key")
	require.NoError(t, err)
	require.Equal(t, sid, got.SessionID)

	// A failed session releases its key so a keyed retry admits a fresh
	// session instead of deduping against the dead one.
	record.Status = OORSessionStatusFailed
	require.NoError(t, store.UpsertSession(ctx, record))

	_, err = store.LookupActiveSessionByIdempotencyKey(ctx, "the-key")
	require.ErrorIs(t, err, ErrOORSessionNotFound)
}

// TestOORSessionRegistryIncomingUpsertPreservesIdempotencyKey verifies that a
// sender receiving its own OOR package cannot erase the outgoing dispatch
// identity when both lifecycle directions share one session registry row.
func TestOORSessionRegistryIncomingUpsertPreservesIdempotencyKey(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newOORSessionRegistryStoreForTest(t)

	sid := sessionHash(0x41)
	outgoing := OORSessionRegistryRecord{
		SessionID:      sid,
		ActorID:        "actor-shared-session",
		Direction:      OORSessionDirectionOutgoing,
		Phase:          "submit_sent",
		IdempotencyKey: "durable-dispatch-key",
		Status:         OORSessionStatusPending,
		SnapshotData: []byte{
			0x01,
		},
		SnapshotVersion: 1,
	}
	outgoing.OutgoingSnapshotData = outgoing.SnapshotData
	outgoing.OutgoingSnapshotVersion = outgoing.SnapshotVersion
	require.NoError(t, store.UpsertSession(ctx, outgoing))

	// A same-session write cannot replace the first non-empty binding and
	// thereby free the original dispatch key for a duplicate transfer.
	rebound := outgoing
	rebound.IdempotencyKey = "replacement-dispatch-key"
	require.NoError(t, store.UpsertSession(ctx, rebound))

	stored, err := store.GetSession(ctx, sid)
	require.NoError(t, err)
	require.Equal(t, outgoing.IdempotencyKey, stored.IdempotencyKey)

	_, err = store.LookupActiveSessionByIdempotencyKey(
		ctx, rebound.IdempotencyKey,
	)
	require.ErrorIs(t, err, ErrOORSessionNotFound)

	// The terminal outgoing snapshot omits recipient artifacts by design.
	// Advancing to it must retain the first artifact-rich snapshot as
	// proof.
	terminal := outgoing
	terminal.Phase = "completed"
	terminal.Status = OORSessionStatusCompleted
	terminal.SnapshotData = []byte{0xff}
	terminal.SnapshotVersion = 5
	terminal.OutgoingSnapshotData = nil
	terminal.OutgoingSnapshotVersion = 0
	require.NoError(t, store.UpsertSession(ctx, terminal))

	// The real incoming lifecycle has no caller idempotency key, but it
	// upserts the same session id after the sender observes its change.
	incoming := OORSessionRegistryRecord{
		SessionID: sid,
		ActorID:   outgoing.ActorID,
		Direction: OORSessionDirectionIncoming,
		Phase:     "completed",
		Status:    OORSessionStatusCompleted,
		SnapshotData: []byte{
			0x02,
		},
		SnapshotVersion: 1,
	}
	require.NoError(t, store.UpsertSession(ctx, incoming))

	stored, err = store.GetSession(ctx, sid)
	require.NoError(t, err)
	require.Equal(t, OORSessionDirectionIncoming, stored.Direction)
	require.Equal(t, incoming.SnapshotData, stored.SnapshotData)
	require.Equal(t, outgoing.IdempotencyKey, stored.IdempotencyKey)
	require.Equal(t, outgoing.SnapshotData, stored.OutgoingSnapshotData)
	require.Equal(
		t, outgoing.SnapshotVersion, stored.OutgoingSnapshotVersion,
	)

	// A retry must still resolve the original session instead of admitting
	// a second transfer after the incoming upsert.
	deduped, err := store.LookupActiveSessionByIdempotencyKey(
		ctx, outgoing.IdempotencyKey,
	)
	require.NoError(t, err)
	require.Equal(t, sid, deduped.SessionID)

	// A failure in the separate incoming lifecycle cannot release a key for
	// an outgoing transfer that already completed successfully.
	incoming.Status = OORSessionStatusFailed
	require.NoError(t, store.UpsertSession(ctx, incoming))

	deduped, err = store.LookupActiveSessionByIdempotencyKey(
		ctx, outgoing.IdempotencyKey,
	)
	require.NoError(t, err)
	require.Equal(t, sid, deduped.SessionID)
}

// TestOORSessionRegistryIdempotencyKeyUniqueIndex verifies the partial UNIQUE
// index: two live rows can never carry the same idempotency key, while a
// failed row drops out of the index so a retry row can reuse its key.
func TestOORSessionRegistryIdempotencyKeyUniqueIndex(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newOORSessionRegistryStoreForTest(t)

	keyedRecord := func(sid chainhash.Hash,
		status OORSessionStatus) OORSessionRegistryRecord {

		return OORSessionRegistryRecord{
			SessionID:      sid,
			ActorID:        "actor-" + sid.String(),
			Direction:      OORSessionDirectionOutgoing,
			Phase:          "submit_sent",
			IdempotencyKey: "shared-key",
			Status:         status,
			SnapshotData: []byte{
				0x01,
			},
			SnapshotVersion: 1,
		}
	}

	first := sessionHash(0x60)
	require.NoError(
		t,
		store.UpsertSession(
			ctx, keyedRecord(first, OORSessionStatusPending),
		),
	)

	// A second live row with the same key violates the dedup invariant
	// and must be rejected by the schema.
	second := sessionHash(0x61)
	err := store.UpsertSession(
		ctx, keyedRecord(second, OORSessionStatusPending),
	)
	require.Error(t, err)

	// Once the first session fails, its key leaves the partial index and
	// the retry row inserts cleanly.
	require.NoError(
		t,
		store.UpsertSession(
			ctx, keyedRecord(first, OORSessionStatusFailed),
		),
	)
	require.NoError(
		t,
		store.UpsertSession(
			ctx, keyedRecord(second, OORSessionStatusPending),
		),
	)
}

// TestOORSessionRegistryTerminalUpsert verifies the production terminal
// mechanism: a snapshot upsert carrying a terminal status updates the row and
// removes it from the non-terminal restore set.
func TestOORSessionRegistryTerminalUpsert(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newOORSessionRegistryStoreForTest(t)

	sid := sessionHash(0x50)
	record := OORSessionRegistryRecord{
		SessionID: sid,
		ActorID:   "actor-term",
		Direction: OORSessionDirectionOutgoing,
		Phase:     "finalize_sent",
		Status:    OORSessionStatusPending,
		SnapshotData: []byte{
			0x01,
		},
		SnapshotVersion: 1,
	}
	require.NoError(t, store.UpsertSession(ctx, record))

	// The terminal turn re-upserts the snapshot with the terminal status
	// derived from the FSM phase; there is no dedicated terminal write.
	record.Status = OORSessionStatusFailed
	record.Phase = "failed"
	record.LastError = "server rejected"
	require.NoError(t, store.UpsertSession(ctx, record))

	got, err := store.GetSession(ctx, sid)
	require.NoError(t, err)
	require.Equal(t, OORSessionStatusFailed, got.Status)
	require.Equal(t, "failed", got.Phase)
	require.Equal(t, "server rejected", got.LastError)

	rows, err := store.ListNonTerminal(ctx)
	require.NoError(t, err)
	require.Empty(t, rows)
}
