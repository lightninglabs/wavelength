package vtxo

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestInMemoryStoreLifecycle asserts basic lifecycle transitions.
func TestInMemoryStoreLifecycle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := NewInMemoryStore()

	op := wire.OutPoint{Hash: chainhash.Hash{9}, Index: 3}

	err := store.Create(ctx, &Record{
		Outpoint: op,
		Value:    1000,
		PkScript: []byte{0x51},
		Status:   StatusLive,
	})
	require.NoError(t, err)

	owner := LockOwner("oor:aaa")
	err = store.MarkInFlight(ctx, []wire.OutPoint{op}, owner)
	require.NoError(t, err)

	rec, err := store.Get(ctx, op)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, StatusInFlight, rec.Status)
	require.Equal(t, owner, rec.InFlightOwner)

	// Idempotent in-flight by same owner.
	err = store.MarkInFlight(ctx, []wire.OutPoint{op}, owner)
	require.NoError(t, err)

	// Spent marks spent and clears owner.
	err = store.MarkSpent(ctx, []wire.OutPoint{op}, owner)
	require.NoError(t, err)

	rec, err = store.Get(ctx, op)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, StatusSpent, rec.Status)
	require.Equal(t, LockOwner(""), rec.InFlightOwner)

	// Idempotent spent.
	err = store.MarkSpent(ctx, []wire.OutPoint{op}, owner)
	require.NoError(t, err)
}

// TestInMemoryStoreInFlightOwnerConflict asserts in-flight owner conflicts are
// rejected.
func TestInMemoryStoreInFlightOwnerConflict(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := NewInMemoryStore()

	op := wire.OutPoint{Hash: chainhash.Hash{9}, Index: 3}
	err := store.Create(ctx, &Record{
		Outpoint: op,
		Value:    1,
		PkScript: []byte{0x51},
		Status:   StatusLive,
	})
	require.NoError(t, err)

	err = store.MarkInFlight(ctx, []wire.OutPoint{op}, LockOwner("oor:a"))
	require.NoError(t, err)

	err = store.MarkInFlight(ctx, []wire.OutPoint{op}, LockOwner("oor:b"))
	require.Error(t, err)
}

// TestInMemoryStoreRejectsDuplicateOutpoints asserts callers cannot pass the
// same outpoint more than once in a single lifecycle call.
func TestInMemoryStoreRejectsDuplicateOutpoints(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := NewInMemoryStore()

	op := wire.OutPoint{Hash: chainhash.Hash{9}, Index: 3}
	err := store.Create(ctx, &Record{
		Outpoint: op,
		Value:    1000,
		PkScript: []byte{0x51},
		Status:   StatusLive,
	})
	require.NoError(t, err)

	err = store.MarkInFlight(ctx, []wire.OutPoint{op, op},
		LockOwner("oor:a"))
	require.ErrorContains(t, err, "duplicate outpoint")

	rec, err := store.Get(ctx, op)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, StatusLive, rec.Status)

	err = store.MarkSpent(
		ctx, []wire.OutPoint{op, op}, LockOwner("oor:a"),
	)
	require.ErrorContains(t, err, "duplicate outpoint")

	rec, err = store.Get(ctx, op)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, StatusLive, rec.Status)
}

// TestInMemoryStoreMarkSpentOwnerEnforcement asserts spent transition requires
// matching in-flight ownership.
func TestInMemoryStoreMarkSpentOwnerEnforcement(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := NewInMemoryStore()

	op := wire.OutPoint{Hash: chainhash.Hash{9}, Index: 3}
	err := store.Create(ctx, &Record{
		Outpoint: op,
		Value:    1000,
		PkScript: []byte{0x51},
		Status:   StatusLive,
	})
	require.NoError(t, err)

	// Cannot spend directly from live.
	err = store.MarkSpent(ctx, []wire.OutPoint{op}, LockOwner("oor:a"))
	require.ErrorContains(t, err, "not spendable (live)")

	err = store.MarkInFlight(ctx, []wire.OutPoint{op}, LockOwner("oor:a"))
	require.NoError(t, err)

	// Wrong owner cannot finalize.
	err = store.MarkSpent(ctx, []wire.OutPoint{op}, LockOwner("oor:b"))
	require.ErrorContains(t, err, "in-flight by")

	// Correct owner finalizes.
	err = store.MarkSpent(ctx, []wire.OutPoint{op}, LockOwner("oor:a"))
	require.NoError(t, err)
}

// TestInMemoryStoreCreateIdempotency asserts Create is idempotent for identical
// records and rejects conflicting re-creates.
func TestInMemoryStoreCreateIdempotency(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := NewInMemoryStore()

	op := wire.OutPoint{Hash: chainhash.Hash{7}, Index: 2}
	record := &Record{
		Outpoint: op,
		Value:    1000,
		PkScript: []byte{0x51},
		Status:   StatusLive,
	}

	err := store.Create(ctx, record)
	require.NoError(t, err)

	// Identical create is idempotent.
	err = store.Create(ctx, &Record{
		Outpoint: op,
		Value:    1000,
		PkScript: []byte{0x51},
		Status:   StatusLive,
	})
	require.NoError(t, err)

	// Conflicting create is rejected.
	err = store.Create(ctx, &Record{
		Outpoint: op,
		Value:    1001,
		PkScript: []byte{0x51},
		Status:   StatusLive,
	})
	require.ErrorContains(t, err, "already exists")
}
