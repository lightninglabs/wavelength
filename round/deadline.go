package round

import (
	"context"
	"time"
)

// AdmissionDeadline is the durable budget of one accepted round attempt.
// Closed attempts cannot be readmitted, even when their deadline is future.
// A deadline never authorizes release of checkpointed input signatures.
type AdmissionDeadline struct {
	// ExpiresAt is an absolute UTC instant, preserved across retries.
	ExpiresAt time.Time

	// Closed records terminal or restart-abandoned admission.
	Closed bool
}

// AdmissionDeadlineStore persists acceptance before the client advances its
// interactive state. Signing sessions remain ephemeral: restart abandons these
// attempts and the existing checkpoint-aware recovery owns reservation cleanup.
type AdmissionDeadlineStore interface {
	// ConstrainAdmissionDeadline creates a deadline or shortens an existing
	// one. It returns the durable winner and never reopens a closed
	// attempt.
	ConstrainAdmissionDeadline(ctx context.Context, roundID RoundID,
		expiresAt time.Time) (AdmissionDeadline, error)

	// CloseAdmissionDeadline idempotently prevents this attempt's revival.
	// It does not release reservations or change a round checkpoint.
	CloseAdmissionDeadline(ctx context.Context, roundID RoundID) error

	// AbandonAdmissionDeadlines closes interrupted attempts on startup.
	// Ephemeral signing state cannot resume; checkpointed rounds instead
	// resume from RoundStore through the existing reconciliation path.
	AbandonAdmissionDeadlines(ctx context.Context) error
}
