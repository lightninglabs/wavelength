package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// hashID returns a 32-byte session id with the supplied seed in the
// last byte. Used to produce cheap distinct session ids for the driver
// tests without dragging in a full crypto setup.
func hashID(seed byte) []byte {
	id := make([]byte, 32)
	id[31] = seed
	return id
}

// TestWalkOORSessionAncestryDriverCycleShortCircuits asserts that a
// back-edge in the persisted store (A → B → A) terminates at the second
// visit of A rather than infinite-recursing. This is the load-bearing
// safety guarantee the shared driver provides on top of the per-call
// session cache: caches avoid duplicate fetches but do not break
// recursion frames.
func TestWalkOORSessionAncestryDriverCycleShortCircuits(t *testing.T) {
	t.Parallel()

	a := hashID(0x0a)
	b := hashID(0x0b)

	// Persisted graph: A → B → A.
	parents := map[string][][]byte{
		string(a): {b},
		string(b): {a},
	}

	visits := make([]string, 0)
	pre := func(_ context.Context, curID []byte,
		_ int) ([][]byte, error) {

		visits = append(visits, fmt.Sprintf("%x", curID[31:]))
		return parents[string(curID)], nil
	}

	err := walkOORSessionAncestryDriver(
		context.Background(), a, pre, nil,
	)
	require.NoError(t, err, "cycle must short-circuit, not error")

	// The driver must visit each session exactly once: A then B. A
	// reappearing as a parent of B is ignored via the seen set.
	require.Equal(t, []string{"0a", "0b"}, visits)
}

// TestWalkOORSessionAncestryDriverDepthBound asserts that a chain
// exceeding DefaultMaxLineageDepth hard-errors with the offending
// session id rather than silently truncating or stack-overflowing. A
// griefer who can persist a deep ancestry chain otherwise inflates
// every cap-arithmetic call into worst-case CPU; the bound caps the
// blast radius.
func TestWalkOORSessionAncestryDriverDepthBound(t *testing.T) {
	t.Parallel()

	// Build a linear chain of DefaultMaxLineageDepth+2 sessions so
	// the walker exceeds the bound on the next-to-last hop.
	parents := make(map[string][][]byte)
	chainLen := DefaultMaxLineageDepth + 2
	ids := make([][]byte, chainLen)
	for i := 0; i < chainLen; i++ {
		ids[i] = make([]byte, 32)
		ids[i][30] = byte(i >> 8)
		ids[i][31] = byte(i)
	}
	for i := 0; i < chainLen-1; i++ {
		parents[string(ids[i])] = [][]byte{ids[i+1]}
	}

	pre := func(_ context.Context, curID []byte,
		_ int) ([][]byte, error) {

		return parents[string(curID)], nil
	}

	err := walkOORSessionAncestryDriver(
		context.Background(), ids[0], pre, nil,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(),
		fmt.Sprintf("max depth %d", DefaultMaxLineageDepth))
}

// TestWalkOORSessionAncestryDriverPropagatesPreError asserts that a
// pre-visitor error short-circuits the walk and propagates verbatim.
// This is the route the cap-arithmetic path uses to surface
// persisted-PSBT parse failures (M-1) and store lookup failures
// without silently under-counting.
func TestWalkOORSessionAncestryDriverPropagatesPreError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("synthetic parse failure")
	pre := func(_ context.Context, _ []byte,
		_ int) ([][]byte, error) {

		return nil, sentinel
	}

	err := walkOORSessionAncestryDriver(
		context.Background(), hashID(0x42), pre, nil,
	)
	require.ErrorIs(t, err, sentinel)
}

// TestWalkOORSessionAncestryDriverDiamondVisitedOnce asserts that a
// diamond-shaped DAG (two distinct children share a parent) visits the
// shared parent exactly once. Without the seen set the cap-arithmetic
// path would double-count overlapping ancestors and exhibit
// O(K^D) blow-up on adversarially-shaped DAGs.
func TestWalkOORSessionAncestryDriverDiamondVisitedOnce(t *testing.T) {
	t.Parallel()

	// Diamond: root → {b, c} → d.
	root := hashID(0x01)
	b := hashID(0x02)
	c := hashID(0x03)
	d := hashID(0x04)

	parents := map[string][][]byte{
		string(root): {b, c},
		string(b):    {d},
		string(c):    {d},
	}

	visitCounts := make(map[string]int)
	pre := func(_ context.Context, curID []byte,
		_ int) ([][]byte, error) {

		visitCounts[string(curID)]++
		return parents[string(curID)], nil
	}

	err := walkOORSessionAncestryDriver(
		context.Background(), root, pre, nil,
	)
	require.NoError(t, err)

	require.Equal(t, 1, visitCounts[string(root)])
	require.Equal(t, 1, visitCounts[string(b)])
	require.Equal(t, 1, visitCounts[string(c)])
	require.Equal(t, 1, visitCounts[string(d)],
		"shared parent must be visited exactly once across both "+
			"children to prevent O(K^D) DoS amplification")
}

// TestWalkOORSessionAncestryDriverPostOrderRunsAfterParents asserts
// that the post visitor fires only after all parents have been
// recursively walked. The recipient-events path relies on this to
// emit packages in dependency order (deepest-first).
func TestWalkOORSessionAncestryDriverPostOrderRunsAfterParents(t *testing.T) {
	t.Parallel()

	// Linear chain root → a → b.
	root := hashID(0x10)
	a := hashID(0x11)
	b := hashID(0x12)
	parents := map[string][][]byte{
		string(root): {a},
		string(a):    {b},
	}

	pre := func(_ context.Context, curID []byte,
		_ int) ([][]byte, error) {

		return parents[string(curID)], nil
	}

	postOrder := make([]string, 0)
	post := func(_ context.Context, curID []byte, _ int) error {
		postOrder = append(postOrder, fmt.Sprintf("%x", curID[31:]))
		return nil
	}

	err := walkOORSessionAncestryDriver(
		context.Background(), root, pre, post,
	)
	require.NoError(t, err)

	// Deepest first: b, then a, then root.
	require.Equal(t, []string{"12", "11", "10"}, postOrder)
}
