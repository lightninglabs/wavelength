package tree

// Queue implements a generic FIFO queue with amortized O(1) Enqueue and
// Dequeue operations. The zero value is ready to use.
//
// This is a simple slice-based implementation suitable for BFS traversal and
// other scenarios where elements are added to the back and removed from the
// front in order.
type Queue[T any] struct {
	items []T
}

// NewQueue creates a new empty queue with optional initial capacity. If no
// capacity is specified, the queue starts with zero capacity and grows as
// needed.
func NewQueue[T any](capacity ...int) *Queue[T] {
	initialCap := 0
	if len(capacity) > 0 {
		initialCap = capacity[0]
	}

	return &Queue[T]{
		items: make([]T, 0, initialCap),
	}
}

// Enqueue adds an item to the back of the queue. This operation has amortized
// O(1) complexity due to the underlying slice's growth strategy.
func (q *Queue[T]) Enqueue(item T) {
	q.items = append(q.items, item)
}

// Dequeue removes and returns the item at the front of the queue. Returns the
// item and true if successful, or the zero value and false if the queue is
// empty.
//
// Note: This implementation does not shrink the underlying slice, so memory
// usage grows with the maximum queue size reached during the queue's lifetime.
// For most use cases (like BFS traversal), this is acceptable.
func (q *Queue[T]) Dequeue() (T, bool) {
	if len(q.items) == 0 {
		var zero T
		return zero, false
	}

	item := q.items[0]
	q.items = q.items[1:]

	return item, true
}

// Peek returns the item at the front of the queue without removing it. Returns
// the item and true if successful, or the zero value and false if the queue is
// empty.
func (q *Queue[T]) Peek() (T, bool) {
	if len(q.items) == 0 {
		var zero T
		return zero, false
	}

	return q.items[0], true
}

// Len returns the number of items currently in the queue.
func (q *Queue[T]) Len() int {
	return len(q.items)
}

// IsEmpty returns true if the queue contains no items.
func (q *Queue[T]) IsEmpty() bool {
	return len(q.items) == 0
}

// Clear removes all items from the queue, resetting it to empty. The
// underlying capacity is preserved.
func (q *Queue[T]) Clear() {
	q.items = q.items[:0]
}
