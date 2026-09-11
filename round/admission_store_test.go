package round

import (
	"context"
	"sync"
	"time"
)

// memoryAdmissionStore models durable deadline semantics for actor tests.
// Sharing it across actors represents restarting against the same database.
type memoryAdmissionStore struct {
	mu        sync.Mutex
	deadlines map[RoundID]AdmissionDeadline
}

// ConstrainAdmissionDeadline saves the earliest budget under the store lock.
func (m *memoryAdmissionStore) ConstrainAdmissionDeadline(_ context.Context,
	id RoundID, expiry time.Time) (AdmissionDeadline, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.deadlines == nil {
		m.deadlines = make(map[RoundID]AdmissionDeadline)
	}
	// Match the database representation, including its finite range.
	expiry = time.Unix(0, expiry.UnixNano()).UTC()
	d, ok := m.deadlines[id]
	if !ok || expiry.Before(d.ExpiresAt) {
		d.ExpiresAt = expiry
	}
	m.deadlines[id] = d

	return d, nil
}

// CloseAdmissionDeadline idempotently fences the named attempt.
func (m *memoryAdmissionStore) CloseAdmissionDeadline(_ context.Context,
	id RoundID) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	if d, ok := m.deadlines[id]; ok {
		d.Closed = true
		m.deadlines[id] = d
	}

	return nil
}

// AbandonAdmissionDeadlines models the startup fence for ephemeral sessions.
func (m *memoryAdmissionStore) AbandonAdmissionDeadlines(
	_ context.Context) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	for id, d := range m.deadlines {
		d.Closed = true
		m.deadlines[id] = d
	}

	return nil
}
