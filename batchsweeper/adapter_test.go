package batchsweeper

import (
	"context"
	"sync"
	"testing"

	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/stretchr/testify/require"
)

// msgCaptureRef is a test double that captures all messages sent to it.
type msgCaptureRef struct {
	mu   sync.Mutex
	msgs []Msg
}

// ID returns the ID of the test double.
func (r *msgCaptureRef) ID() string {
	return "msg-capture"
}

// Tell captures the message.
func (r *msgCaptureRef) Tell(_ context.Context, msg Msg) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.msgs = append(r.msgs, msg)
}

// Messages returns a copy of all captured messages.
func (r *msgCaptureRef) Messages() []Msg {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]Msg(nil), r.msgs...)
}

// TestMapBatchWatcherNotification verifies that BatchWatcher notifications are
// wrapped and forwarded as internal BatchSweeper messages.
func TestMapBatchWatcherNotification(t *testing.T) {
	t.Parallel()

	capture := &msgCaptureRef{}
	ref := MapBatchWatcherNotification(capture)

	expiry := &batchwatcher.BatchExpiredNotification{}
	ref.Tell(t.Context(), expiry)

	treeChanged := &batchwatcher.TreeStateChangedNotification{}
	ref.Tell(t.Context(), treeChanged)

	msgs := capture.Messages()
	require.Len(t, msgs, 2)

	expiryEvent, ok := msgs[0].(*BatchExpiredEvent)
	require.True(t, ok)
	require.Same(t, expiry, expiryEvent.Notification)

	treeEvent, ok := msgs[1].(*TreeStateChangedEvent)
	require.True(t, ok)
	require.Same(t, treeChanged, treeEvent.Notification)
}
