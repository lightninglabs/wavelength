package fraud

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestAncestorWatchesRefcounting verifies the refcounted lifecycle:
// the first addTarget reports firstRefFor=true, subsequent retains by
// other targets do not, and dropTarget only signals "empty" once the
// last target releases.
func TestAncestorWatchesRefcounting(t *testing.T) {
	t.Parallel()

	w := newAncestorWatches()

	op := wire.OutPoint{Hash: [32]byte{0xaa}}
	point := WatchPoint{
		Outpoint: op,
		PkScript: []byte{
			0x51,
			0x20,
		},
		HeightHint: 100,
	}
	targetA := wire.OutPoint{Hash: [32]byte{0xa1}}
	targetB := wire.OutPoint{Hash: [32]byte{0xb1}}

	require.True(
		t, w.firstRefFor(op),
		"empty collection must report first reference",
	)

	w.addTarget(point, targetA)
	require.False(
		t, w.firstRefFor(op),
		"after addTarget the outpoint is tracked",
	)
	require.Equal(t, map[wire.OutPoint]struct{}{
		targetA: {},
	}, w.targetsOf(op))

	w.addTarget(point, targetB)
	require.Equal(t, map[wire.OutPoint]struct{}{
		targetA: {},
		targetB: {},
	}, w.targetsOf(op))

	// Re-adding the same target is idempotent.
	w.addTarget(point, targetA)
	require.Len(t, w.targetsOf(op), 2)

	// Drop the first target: watch must stay armed.
	gotPoint, emptied := w.dropTarget(op, targetA)
	require.False(
		t, emptied,
		"watch must remain armed while other targets reference it",
	)
	require.Equal(t, WatchPoint{}, gotPoint)
	require.Equal(t, map[wire.OutPoint]struct{}{
		targetB: {},
	}, w.targetsOf(op))

	// Drop the last target: collection returns the stored point so
	// the caller can unregister, and the entry is removed.
	gotPoint, emptied = w.dropTarget(op, targetB)
	require.True(
		t, emptied,
		"watch must signal empty when the last target releases",
	)
	require.Equal(
		t, point, gotPoint,
		"stored WatchPoint must round-trip to the caller",
	)
	require.Nil(
		t, w.targetsOf(op),
		"removed entry must not appear via targetsOf",
	)
	require.True(
		t, w.firstRefFor(op),
		"after the last release, the outpoint is fresh again",
	)
}

// TestAncestorWatchesDropTargetMissing verifies that dropTarget on an
// untracked outpoint is a quiet no-op rather than a panic.
func TestAncestorWatchesDropTargetMissing(t *testing.T) {
	t.Parallel()

	w := newAncestorWatches()
	op := wire.OutPoint{Hash: [32]byte{0xaa}}
	target := wire.OutPoint{Hash: [32]byte{0xa1}}

	gotPoint, emptied := w.dropTarget(op, target)
	require.False(t, emptied)
	require.Equal(t, WatchPoint{}, gotPoint)
}

// TestAncestorWatchesOutpointsAndPointAt verifies the iteration and
// lookup helpers used by the shutdown path (OnStop must walk every
// tracked outpoint and read its stored point without dropping the
// entry).
func TestAncestorWatchesOutpointsAndPointAt(t *testing.T) {
	t.Parallel()

	w := newAncestorWatches()

	opA := wire.OutPoint{Hash: [32]byte{0x01}}
	opB := wire.OutPoint{Hash: [32]byte{0x02}}
	pointA := WatchPoint{Outpoint: opA, HeightHint: 100}
	pointB := WatchPoint{Outpoint: opB, HeightHint: 200}
	target := wire.OutPoint{Hash: [32]byte{0xff}}

	w.addTarget(pointA, target)
	w.addTarget(pointB, target)

	require.ElementsMatch(
		t, []wire.OutPoint{opA, opB}, w.outpoints(),
	)

	gotA, ok := w.pointAt(opA)
	require.True(t, ok)
	require.Equal(t, pointA, gotA)

	gotB, ok := w.pointAt(opB)
	require.True(t, ok)
	require.Equal(t, pointB, gotB)

	_, ok = w.pointAt(wire.OutPoint{Hash: [32]byte{0x09}})
	require.False(t, ok)

	// pointAt does not drop the entry — the outpoints list must
	// still report both after multiple reads.
	require.ElementsMatch(
		t, []wire.OutPoint{opA, opB}, w.outpoints(),
	)
}
