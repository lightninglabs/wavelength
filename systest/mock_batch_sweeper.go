//go:build systest

package systest

import (
	"context"
	"sync"

	"github.com/lightninglabs/darepo/batchwatcher"
)

// MockBatchSweeper is a test double for the BatchSweeper actor that captures
// all notifications sent to it. This allows tests to verify that the
// BatchWatcher correctly sends expiry and tree state change notifications.
// It implements actor.TellOnlyRef[batchwatcher.BatchSweeperMsg].
type MockBatchSweeper struct {
	mu sync.Mutex

	// expiryNotifications stores all BatchExpiredNotification messages
	// received, keyed by BatchID string for easy lookup.
	expiryNotifications map[string]*batchwatcher.BatchExpiredNotification

	// treeStateNotifications stores all TreeStateChangedNotification messages
	// received, keyed by BatchID string.
	treeStateNotifications map[string][]*batchwatcher.TreeStateChangedNotification
}

// ID implements actor.BaseActorRef for the mock.
func (m *MockBatchSweeper) ID() string {
	return "mock-batch-sweeper"
}

// NewMockBatchSweeper creates a new mock batch sweeper.
func NewMockBatchSweeper() *MockBatchSweeper {
	return &MockBatchSweeper{
		expiryNotifications:    make(map[string]*batchwatcher.BatchExpiredNotification),
		treeStateNotifications: make(map[string][]*batchwatcher.TreeStateChangedNotification),
	}
}

// Tell implements the actor.TellOnlyRef interface for BatchSweeperMsg.
func (m *MockBatchSweeper) Tell(_ context.Context,
	msg batchwatcher.BatchSweeperMsg) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	switch notification := msg.(type) {
	case *batchwatcher.BatchExpiredNotification:
		batchIDStr := notification.BatchID.String()
		m.expiryNotifications[batchIDStr] = notification

	case *batchwatcher.TreeStateChangedNotification:
		batchIDStr := notification.BatchID.String()
		m.treeStateNotifications[batchIDStr] = append(
			m.treeStateNotifications[batchIDStr], notification,
		)
	}

	return nil
}

// GetExpiryNotification returns the expiry notification for a batch, if any.
func (m *MockBatchSweeper) GetExpiryNotification(
	batchID batchwatcher.BatchID) *batchwatcher.BatchExpiredNotification {

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.expiryNotifications[batchID.String()]
}

// HasExpiryNotification returns true if an expiry notification was received
// for the given batch.
func (m *MockBatchSweeper) HasExpiryNotification(
	batchID batchwatcher.BatchID) bool {

	m.mu.Lock()
	defer m.mu.Unlock()

	_, ok := m.expiryNotifications[batchID.String()]
	return ok
}

// ExpiryNotificationCount returns the total number of expiry notifications
// received.
func (m *MockBatchSweeper) ExpiryNotificationCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.expiryNotifications)
}

// GetTreeStateNotifications returns all tree state change notifications for a
// batch.
func (m *MockBatchSweeper) GetTreeStateNotifications(
	batchID batchwatcher.BatchID) []*batchwatcher.TreeStateChangedNotification {

	m.mu.Lock()
	defer m.mu.Unlock()

	notifications := m.treeStateNotifications[batchID.String()]
	if notifications == nil {
		return nil
	}

	// Return a copy to prevent external modification.
	result := make(
		[]*batchwatcher.TreeStateChangedNotification, len(notifications),
	)
	copy(result, notifications)

	return result
}

// Clear resets all captured notifications.
func (m *MockBatchSweeper) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.expiryNotifications = make(
		map[string]*batchwatcher.BatchExpiredNotification,
	)
	m.treeStateNotifications = make(
		map[string][]*batchwatcher.TreeStateChangedNotification,
	)
}
