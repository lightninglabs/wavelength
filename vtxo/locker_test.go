package vtxo

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestInMemoryLockerMutualExclusion asserts that the in-memory locker enforces
// mutual exclusion between different owners and is idempotent for the same
// owner.
func TestInMemoryLockerMutualExclusion(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	locker := NewInMemoryLocker()

	outpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			1,
			2,
			3,
		},
		Index: 7,
	}

	ownerA := LockOwner("oor:aaa")
	ownerB := LockOwner("round:bbb")

	err := locker.LockMany(ctx, []wire.OutPoint{outpoint}, ownerA)
	require.NoError(t, err)

	// Idempotent re-lock by same owner.
	err = locker.LockMany(ctx, []wire.OutPoint{outpoint}, ownerA)
	require.NoError(t, err)

	// Lock by different owner fails.
	err = locker.LockMany(ctx, []wire.OutPoint{outpoint}, ownerB)
	require.Error(t, err)
	var lockedErr *ErrLocked
	require.ErrorAs(t, err, &lockedErr)

	// Unlock by wrong owner fails.
	err = locker.UnlockMany(ctx, []wire.OutPoint{outpoint}, ownerB)
	require.Error(t, err)
	var notOwnerErr *ErrNotOwner
	require.ErrorAs(t, err, &notOwnerErr)

	// Unlock by owner succeeds.
	err = locker.UnlockMany(ctx, []wire.OutPoint{outpoint}, ownerA)
	require.NoError(t, err)

	// Idempotent unlock.
	err = locker.UnlockMany(ctx, []wire.OutPoint{outpoint}, ownerA)
	require.NoError(t, err)
}

// TestInMemoryLockerAtomicLockMany asserts that LockMany is all-or-nothing.
func TestInMemoryLockerAtomicLockMany(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	locker := NewInMemoryLocker()

	a := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0}
	b := wire.OutPoint{Hash: chainhash.Hash{2}, Index: 1}

	err := locker.LockMany(ctx, []wire.OutPoint{a}, LockOwner("oor:a"))
	require.NoError(t, err)

	// Attempt to lock {a,b} with a different owner should fail and not
	// lock b.
	err = locker.LockMany(ctx, []wire.OutPoint{a, b}, LockOwner("oor:b"))
	require.Error(t, err)

	// Confirm b is not locked by trying to lock it with another owner.
	err = locker.LockMany(ctx, []wire.OutPoint{b}, LockOwner("oor:c"))
	require.NoError(t, err)
}

// TestInMemoryLockerExpiry asserts that leases expire and are treated as
// released.
func TestInMemoryLockerExpiry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	start := time.Unix(1700000000, 0).UTC()
	clk := clock.NewTestClock(start)
	locker := NewInMemoryLockerWithClock(clk)

	outpoint := wire.OutPoint{Hash: chainhash.Hash{9}, Index: 2}
	ownerA := LockOwner("oor:aaa")
	ownerB := LockOwner("round:bbb")

	// Acquire a lock with an expiry.
	expiresAt := start.Add(5 * time.Second)
	err := locker.LockManyWithExpiry(
		ctx, []wire.OutPoint{outpoint}, ownerA, expiresAt,
	)
	require.NoError(t, err)

	// Before expiry, another owner cannot acquire it.
	err = locker.LockMany(ctx, []wire.OutPoint{outpoint}, ownerB)
	require.Error(t, err)

	// After expiry, it should be treated as released.
	clk.SetTime(expiresAt.Add(time.Second))

	// Unlock after expiry should be a no-op (the lease is treated as
	// released).
	err = locker.UnlockMany(ctx, []wire.OutPoint{outpoint}, ownerA)
	require.NoError(t, err)

	err = locker.LockMany(ctx, []wire.OutPoint{outpoint}, ownerB)
	require.NoError(t, err)
}

// TestLockOwnerHelpers asserts subsystem-specific owner helpers use stable
// prefixes to avoid collisions.
func TestLockOwnerHelpers(t *testing.T) {
	t.Parallel()

	require.Equal(t, LockOwner("round:abc"), RoundLockOwner("abc"))
	require.Equal(t, LockOwner("oor:def"), OORLockOwner("def"))
}

// TestLockerErrorStrings asserts the concrete error types render useful error
// messages.
func TestLockerErrorStrings(t *testing.T) {
	t.Parallel()

	outpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			1,
		},
		Index: 2,
	}

	errLocked := &ErrLocked{
		Outpoint: outpoint,
		Owner:    LockOwner("oor:session"),
	}
	require.Contains(t, errLocked.Error(), "locked")

	errNotOwner := &ErrNotOwner{
		Outpoint: outpoint,
		Owner:    LockOwner("oor:session"),
		Attempt:  LockOwner("round:round"),
	}
	require.Contains(t, errNotOwner.Error(), "attempt")
}
