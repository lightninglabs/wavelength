package tree

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQueueBasicOperations tests the fundamental enqueue and dequeue
// operations of the Queue.
func TestQueueBasicOperations(t *testing.T) {
	t.Parallel()

	q := NewQueue[int]()

	// Queue should start empty.
	require.True(t, q.IsEmpty())
	require.Equal(t, 0, q.Len())

	// Enqueue some items.
	q.Enqueue(1)
	q.Enqueue(2)
	q.Enqueue(3)

	require.False(t, q.IsEmpty())
	require.Equal(t, 3, q.Len())

	// Dequeue items and verify FIFO order.
	item, ok := q.Dequeue()
	require.True(t, ok)
	require.Equal(t, 1, item)

	item, ok = q.Dequeue()
	require.True(t, ok)
	require.Equal(t, 2, item)

	item, ok = q.Dequeue()
	require.True(t, ok)
	require.Equal(t, 3, item)

	// Queue should be empty now.
	require.True(t, q.IsEmpty())
	require.Equal(t, 0, q.Len())
}

// TestQueueEmptyDequeue verifies that dequeueing from an empty queue returns
// the appropriate zero value and false status.
func TestQueueEmptyDequeue(t *testing.T) {
	t.Parallel()

	q := NewQueue[int]()

	// Dequeue from empty queue.
	item, ok := q.Dequeue()
	require.False(t, ok)
	require.Equal(t, 0, item)

	// Enqueue and dequeue one item.
	q.Enqueue(42)
	item, ok = q.Dequeue()
	require.True(t, ok)
	require.Equal(t, 42, item)

	// Dequeue from now-empty queue.
	item, ok = q.Dequeue()
	require.False(t, ok)
	require.Equal(t, 0, item)
}

// TestQueuePeek verifies that Peek returns the front item without removing it.
func TestQueuePeek(t *testing.T) {
	t.Parallel()

	q := NewQueue[string]()

	// Peek at empty queue.
	item, ok := q.Peek()
	require.False(t, ok)
	require.Equal(t, "", item)

	// Enqueue items.
	q.Enqueue("first")
	q.Enqueue("second")

	// Peek should return first item without removing it.
	item, ok = q.Peek()
	require.True(t, ok)
	require.Equal(t, "first", item)

	// Queue should still have both items.
	require.Equal(t, 2, q.Len())

	// Peek again to verify it's idempotent.
	item, ok = q.Peek()
	require.True(t, ok)
	require.Equal(t, "first", item)

	// Dequeue should remove the peeked item.
	item, ok = q.Dequeue()
	require.True(t, ok)
	require.Equal(t, "first", item)

	// Now peek should show the second item.
	item, ok = q.Peek()
	require.True(t, ok)
	require.Equal(t, "second", item)
}

// TestQueueClear verifies that Clear removes all items from the queue.
func TestQueueClear(t *testing.T) {
	t.Parallel()

	q := NewQueue[int]()

	// Add items.
	q.Enqueue(1)
	q.Enqueue(2)
	q.Enqueue(3)
	require.Equal(t, 3, q.Len())

	// Clear the queue.
	q.Clear()
	require.True(t, q.IsEmpty())
	require.Equal(t, 0, q.Len())

	// Dequeue should return false.
	_, ok := q.Dequeue()
	require.False(t, ok)

	// Queue should be usable after clear.
	q.Enqueue(42)
	item, ok := q.Dequeue()
	require.True(t, ok)
	require.Equal(t, 42, item)
}

// TestQueueFIFOOrdering verifies that items are dequeued in the exact order
// they were enqueued.
func TestQueueFIFOOrdering(t *testing.T) {
	t.Parallel()

	q := NewQueue[int]()

	// Enqueue a sequence of items.
	for i := 0; i < 100; i++ {
		q.Enqueue(i)
	}

	// Dequeue and verify ordering.
	for i := 0; i < 100; i++ {
		item, ok := q.Dequeue()
		require.True(t, ok)
		require.Equal(t, i, item)
	}

	require.True(t, q.IsEmpty())
}

// TestQueueInterleavedOperations tests enqueueing and dequeueing in mixed
// order, simulating real-world usage patterns like BFS traversal.
func TestQueueInterleavedOperations(t *testing.T) {
	t.Parallel()

	q := NewQueue[int]()

	// Enqueue two items.
	q.Enqueue(1)
	q.Enqueue(2)

	// Dequeue one.
	item, ok := q.Dequeue()
	require.True(t, ok)
	require.Equal(t, 1, item)

	// Enqueue two more.
	q.Enqueue(3)
	q.Enqueue(4)

	// Dequeue all remaining.
	item, ok = q.Dequeue()
	require.True(t, ok)
	require.Equal(t, 2, item)

	item, ok = q.Dequeue()
	require.True(t, ok)
	require.Equal(t, 3, item)

	item, ok = q.Dequeue()
	require.True(t, ok)
	require.Equal(t, 4, item)

	require.True(t, q.IsEmpty())
}

// TestQueueWithCapacity verifies that providing an initial capacity works
// correctly and doesn't affect behavior.
func TestQueueWithCapacity(t *testing.T) {
	t.Parallel()

	q := NewQueue[int](10)

	// Behavior should be identical to zero-capacity queue.
	require.True(t, q.IsEmpty())

	q.Enqueue(1)
	q.Enqueue(2)

	item, ok := q.Dequeue()
	require.True(t, ok)
	require.Equal(t, 1, item)

	item, ok = q.Dequeue()
	require.True(t, ok)
	require.Equal(t, 2, item)

	require.True(t, q.IsEmpty())
}

// TestQueueWithStructs verifies that the queue works with struct types, which
// is how it's used in buildTreeBFS with workItem.
func TestQueueWithStructs(t *testing.T) {
	t.Parallel()

	type testItem struct {
		id    int
		value string
	}

	q := NewQueue[testItem]()

	// Enqueue structs.
	q.Enqueue(testItem{id: 1, value: "first"})
	q.Enqueue(testItem{id: 2, value: "second"})

	// Dequeue and verify.
	item, ok := q.Dequeue()
	require.True(t, ok)
	require.Equal(t, 1, item.id)
	require.Equal(t, "first", item.value)

	item, ok = q.Dequeue()
	require.True(t, ok)
	require.Equal(t, 2, item.id)
	require.Equal(t, "second", item.value)

	require.True(t, q.IsEmpty())
}
