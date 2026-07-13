package chainsource

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDrainToLatestEpochCoalesces verifies that a backlog of queued block
// epochs collapses to the most recent one, so finality synthesis evaluates
// the highest observed height rather than re-checking once per stale epoch.
func TestDrainToLatestEpochCoalesces(t *testing.T) {
	t.Parallel()

	ch := make(chan *BlockEpoch, 8)
	cur := &BlockEpoch{Height: 100}

	// Queue several newer epochs behind the one already dequeued.
	for h := int32(101); h <= 105; h++ {
		ch <- &BlockEpoch{Height: h}
	}

	got, closed := drainToLatestEpoch(ch, cur)
	require.False(t, closed)
	require.NotNil(t, got)
	require.Equal(t, int32(105), got.Height)

	// The channel must be fully drained afterwards.
	require.Len(t, ch, 0)
}

// TestDrainToLatestEpochEmpty verifies that with nothing queued the helper
// returns the current epoch unchanged and reports the channel open.
func TestDrainToLatestEpochEmpty(t *testing.T) {
	t.Parallel()

	ch := make(chan *BlockEpoch, 4)
	cur := &BlockEpoch{Height: 42}

	got, closed := drainToLatestEpoch(ch, cur)
	require.False(t, closed)
	require.Same(t, cur, got)
}

// TestDrainToLatestEpochSkipsNil verifies that nil epochs in the backlog are
// ignored: the most recent non-nil epoch wins.
func TestDrainToLatestEpochSkipsNil(t *testing.T) {
	t.Parallel()

	ch := make(chan *BlockEpoch, 4)
	cur := &BlockEpoch{Height: 10}
	ch <- &BlockEpoch{Height: 11}
	ch <- nil

	got, closed := drainToLatestEpoch(ch, cur)
	require.False(t, closed)
	require.NotNil(t, got)
	require.Equal(t, int32(11), got.Height)
}

// TestDrainToLatestEpochClosed verifies that a closed channel is reported so
// the caller can park its receive, while still returning the latest epoch
// observed before the close.
func TestDrainToLatestEpochClosed(t *testing.T) {
	t.Parallel()

	ch := make(chan *BlockEpoch, 4)
	cur := &BlockEpoch{Height: 7}
	ch <- &BlockEpoch{Height: 8}
	close(ch)

	got, closed := drainToLatestEpoch(ch, cur)
	require.True(t, closed)
	require.NotNil(t, got)
	require.Equal(t, int32(8), got.Height)
}
