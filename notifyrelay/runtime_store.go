package notifyrelay

import (
	"context"
	"time"
)

// managementStore is the shared HTTP/delivery transaction boundary. Production
// construction accepts only SQLStore. The retired store remains in library
// regression fixtures until the separate cleanup slice.
type managementStore interface {
	// NewChallenge creates stateless, scope-bound management authority.
	NewChallenge(context.Context, ChallengeRequest,
		time.Time) (Challenge, error)

	// loadChallenge verifies the exact issuer token and expiry.
	loadChallenge(context.Context, string, time.Time) (Challenge, error)

	// enroll consumes a verified challenge with public credential
	// insertion.
	enroll(context.Context, Challenge, installation, time.Time) error

	// loadInstallation restores current public evidence and private
	// destination.
	loadInstallation(context.Context, string) (installation, error)

	// applyManagement commits proof counters, nonce and exact requested
	// mutation.
	applyManagement(context.Context, Challenge, uint32, ManagementPayload,
		time.Time) (ManagementResult, error)

	// claimWithProgress also reports committed deferred-page work.
	claimWithProgress(context.Context, time.Time) (*Delivery, bool, error)

	// complete fences the claim and token revision after provider I/O.
	complete(context.Context, Delivery, ProviderResult, time.Time) error

	// pruneExpired bounds storage independently of producer progress.
	pruneExpired(context.Context, time.Time) error

	// pendingJobs returns only a count for internal metrics.
	pendingJobs(context.Context) (int64, error)
}

// pendingJobs counts durable obligations without exposing route identifiers.
func (s *SQLStore) pendingJobs(ctx context.Context) (int64, error) {
	return s.database.Queries.CountPendingJobs(ctx)
}
