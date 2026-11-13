package notifyrelay

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
)

// enqueueSQLJobTx joins a committed source checkpoint to its local delivery
// obligation. Disabled tokens still retain work; they cannot block a source's
// cumulative ACK. Payment correlators deduplicate; ordinary wakes use the
// source checkpoint and binding generation instead of growing a replay set.
func enqueueSQLJobTx(ctx context.Context, q *sqlc.Queries,
	sub sqlc.RelaySubscription, scope Scope, eventID string,
	now time.Time) error {

	if sub.ExpiresAt <= now.Unix() {
		return ErrNotFound
	}
	if scope != ScopeWake && scope != ScopeAlert &&
		scope != ScopeRound && scope != ScopeSchedule {
		return ErrNotFound
	}
	if scope == ScopeAlert && !sub.AlertsEnabled {
		return nil
	}
	hash := secretHash(eventID)
	replayReserved := false
	if scope == ScopeAlert {
		duplicate, err := sqlPaymentDuplicate(ctx, q, sub, hash, now)
		if err != nil && !errors.Is(err, ErrCapacity) {
			return err
		}
		if duplicate {
			return nil
		}
		// A device replay quota must not block the aggregate source
		// cursor. Keep its bounded job and reserve replay space before
		// provider I/O.
		replayReserved = err == nil
	}
	id := string(scope) + "/" + sub.ID
	_, err := q.GetJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		count, err := q.CountPendingJobs(ctx)
		if err != nil {
			return err
		}
		if count >= maxPendingJobs {
			return ErrCapacity
		}
	} else if err != nil {
		return err
	}
	expiry := min(
		now.Add(jobLifetime).UnixNano(),
		time.Unix(sub.ExpiresAt, 0).UnixNano(),
	)
	if err := q.CoalesceJob(ctx, sqlc.CoalesceJobParams{
		ID: id, SubscriptionID: sub.ID,
		Scope: string(scope), EventHash: hash,
		NextAt: now.UnixNano(), ExpiresAt: expiry,
	}); err != nil {
		return err
	}
	if scope != ScopeAlert || !replayReserved {
		return nil
	}

	return q.PutDedup(ctx, sqlc.PutDedupParams{
		SubscriptionID: sub.ID, InstallationID: sub.InstallationID,
		Scope: string(scope), EventHash: hash, ExpiresAt: now.
			Add(dedupLifetime).
			Unix(),
	})
}

// sqlPaymentDuplicate checks live replay evidence and per-device/global
// capacity. A quota defers provider admission, not durable source ingestion.
func sqlPaymentDuplicate(ctx context.Context, q *sqlc.Queries,
	sub sqlc.RelaySubscription, hash string, now time.Time) (bool, error) {

	expiry, err := q.GetDedup(ctx, sqlc.GetDedupParams{
		SubscriptionID: sub.ID, Scope: string(ScopeAlert),
		EventHash: hash,
	})
	if err == nil && expiry > now.Unix() {
		return true, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	global, err := q.CountDedup(ctx)
	if err != nil {
		return false, err
	}
	owned, err := q.CountInstallationDedup(ctx, sub.InstallationID)
	if err != nil {
		return false, err
	}
	route, err := q.CountSubscriptionDedup(ctx, sub.ID)
	if err != nil {
		return false, err
	}
	if global >= maxDedupRecords || owned >= maxDedupPerInstallation ||
		route >= maxDedupPerRoute {
		return false, ErrCapacity
	}

	return false, nil
}

// claim freezes one provider request and serializes all routes belonging to
// its installation across replicas. A second route cannot bypass quiet periods
// by being claimed before the first route's provider result commits.
func (s *SQLStore) claim(ctx context.Context, now time.Time) (*Delivery,
	error) {

	delivery, _, err := s.claimWithProgress(ctx, now)

	return delivery, err
}

// claimWithProgress reports committed deferral work as well as provider-ready
// work. A worker can inspect the next due page immediately after moving a full
// replay-blocked page forward, without holding a longer writer transaction.
func (s *SQLStore) claimWithProgress(ctx context.Context, now time.Time) (
	*Delivery, bool, error) {

	token, err := randomSecret(16)
	if err != nil {
		return nil, false, err
	}
	var delivery *Delivery
	var progress bool
	err = s.database.Write(ctx, func(q *sqlc.Queries) error {
		delivery = nil
		progress = false
		due, err := q.ListDueJobs(ctx, sqlc.ListDueJobsParams{
			NowNano: now.UnixNano(), NowUnix: now.Unix(),
			BatchLimit: 32,
		})
		if err != nil {
			return err
		}
		for _, candidate := range due {
			sub, err := q.GetSubscription(
				ctx, candidate.SubscriptionID,
			)
			if err != nil {
				return err
			}
			ready, err := prepareSQLPayment(
				ctx, q, candidate, sub, now,
			)
			if err != nil {
				return err
			}
			if !ready {
				progress = true
				continue
			}
			claimed, err := q.ClaimInstallation(
				ctx, sqlc.ClaimInstallationParams{
					ID:         sub.InstallationID,
					ClaimToken: token,
					LeaseUntil: now.
						Add(claimLifetime).
						UnixNano(),
					NowNano: now.UnixNano(),
				},
			)
			if err != nil {
				return err
			}
			if claimed == 0 {
				continue
			}
			job, err := q.ClaimJob(ctx, sqlc.ClaimJobParams{
				ID: candidate.ID, ClaimToken: token,
				LeaseUntil: now.Add(claimLifetime).UnixNano(),
				NowNano:    now.UnixNano(),
			})
			if err != nil {
				return err
			}
			owner, err := loadSQLInstallation(
				ctx, q, sub.InstallationID,
			)
			if err != nil {
				return err
			}
			delivery = sqlDelivery(job, sub, owner)

			return nil
		}

		return nil
	})

	return delivery, progress && err == nil, err
}

// prepareSQLPayment reserves replay evidence before a payment provider call.
// Existing evidence belongs to this pending job/retry: enqueue never creates
// a job for an already admitted identity. Missing evidence means a bounded
// candidate deferred by quota. Delay only that job, leaving other routes free.
func prepareSQLPayment(ctx context.Context, q *sqlc.Queries, job sqlc.RelayJob,
	sub sqlc.RelaySubscription, now time.Time) (bool, error) {

	if job.Scope != string(ScopeAlert) {
		return true, nil
	}
	exists, err := sqlPaymentDuplicate(ctx, q, sub, job.EventHash, now)
	if errors.Is(err, ErrCapacity) {
		return false, q.DeferUnclaimedJob(
			ctx, sqlc.DeferUnclaimedJobParams{
				ID:      job.ID,
				NextAt:  now.Add(time.Minute).UnixNano(),
				NowNano: now.UnixNano(),
			},
		)
	}
	if err != nil {
		return false, err
	}
	if exists {
		return true, nil
	}
	err = q.PutDedup(ctx, sqlc.PutDedupParams{
		SubscriptionID: sub.ID, InstallationID: sub.InstallationID,
		Scope:     string(ScopeAlert),
		EventHash: job.EventHash, ExpiresAt: now.
				Add(dedupLifetime).
				Unix(),
	})

	return err == nil, err
}

// sqlDelivery constructs a fixed route/template from transaction-owned state.
// No source payload controls a device, app identity or notification text.
func sqlDelivery(job sqlc.RelayJob, sub sqlc.RelaySubscription,
	owner installation) *Delivery {

	frozen := relayJob{
		ID: job.ID, SubscriptionID: job.SubscriptionID, Scope: Scope(
			job.Scope,
		),
		Generation: uint64(job.Generation), DoneGeneration: uint64(
			job.DoneGeneration,
		),
		EventHash: job.EventHash, NextAt: job.NextAt,
		ExpiresAt:  job.ExpiresAt,
		ClaimToken: job.ClaimToken, LeaseUntil: job.LeaseUntil,
		Attempts: uint32(
			job.Attempts,
		),
	}

	scope := frozen.Scope
	if scope == ScopeRound || scope == ScopeSchedule {
		scope = ScopeWake
	}

	return &Delivery{
		DeviceToken: owner.DeviceToken, Topic: owner.Policy.Topic,
		Environment: owner.Policy.APNSEnvironment, Scope: scope,
		Presentation: AlertPresentation(sub.Presentation),
		RouteID:      sub.RouteID,
		EventHash:    job.EventHash, CollapseID: secretHash(job.ID),
		ExpiresAt: time.Unix(0, job.ExpiresAt), job: frozen,
		tokenRevision: owner.TokenRevision, policy: owner.Policy,
		installationID: owner.ID,
	}
}

// complete applies one claim-fenced provider outcome and releases its device
// lease in the same transaction. An old owner cannot release a newer claim.
func (s *SQLStore) complete(ctx context.Context, sent Delivery,
	result ProviderResult, now time.Time) error {

	return s.database.Write(ctx, func(q *sqlc.Queries) error {
		if err := completeSQLJob(
			ctx, q, sent, result, now,
		); err != nil {
			return err
		}
		_, err := q.ReleaseInstallation(
			ctx, sqlc.ReleaseInstallationParams{
				ID: sent.installationID,
				DeliveryClaimToken: sent.
					job.
					ClaimToken,
			},
		)

		return err
	})
}

// completeSQLJob preserves newer generations, token rotations and presentation
// cutovers. Invalid destinations keep a bounded job for signed token recovery.
func completeSQLJob(ctx context.Context, q *sqlc.Queries, sent Delivery,
	result ProviderResult, now time.Time) error {

	row, err := q.GetInstallation(ctx, sent.installationID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && row.Deleted {
		return nil
	}
	if err != nil {
		return err
	}
	if row.DeliveryClaimToken != sent.job.ClaimToken {
		return nil
	}
	owner, err := loadSQLInstallation(ctx, q, sent.installationID)
	if err != nil {
		return err
	}
	// An accepted send consumes device capacity even if a concurrent
	// presentation change, revocation or expiry removed its scope job.
	// The installation lease owns this effect; only job retirement below
	// depends on the surviving job claim.
	if result.Outcome == OutcomeAccepted {
		switch sent.job.Scope {
		case ScopeWake:
			owner.NextWakeAt = now.Add(wakeQuietPeriod).UnixNano()

		case ScopeRound:
			owner.NextRoundAt = now.Add(roundQuietPeriod).UnixNano()

		case ScopeSchedule:
			owner.NextScheduleAt = now.
				Add(scheduleQuietPeriod).
				UnixNano()

		case ScopeAlert:
			owner.NextAlertAt = now.Add(alertQuietPeriod).UnixNano()
		}
		if err := saveSQLInstallation(ctx, q, owner); err != nil {
			return err
		}
	}
	job, err := q.GetJob(ctx, sent.job.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if job.ClaimToken != sent.job.ClaimToken {
		return nil
	}
	if result.Outcome == OutcomeInvalidToken {
		current := owner.TokenRevision == sent.tokenRevision
		recent := result.InvalidAt.IsZero() || !result.InvalidAt.Before(
			time.Unix(0, owner.TokenUpdated),
		)
		if current && recent {
			owner.Disabled = true
			if err := saveSQLInstallation(
				ctx, q, owner,
			); err != nil {
				return err
			}

			return finishSQLJob(
				ctx, q, job, job.DoneGeneration, now.UnixNano(),
			)
		}
		result.Outcome = OutcomeRetry
		if current {
			result.Outcome = OutcomeRejected
		}
	}
	done := job.DoneGeneration
	next := now.UnixNano()
	switch result.Outcome {
	case OutcomeAccepted, OutcomeRejected:
		done = max(done, int64(sent.job.Generation))

		if job.Generation == done {
			_, err := q.DeleteClaimedJob(
				ctx, sqlc.DeleteClaimedJobParams{
					ID:         job.ID,
					ClaimToken: job.ClaimToken,
				},
			)

			return err
		}

	case OutcomeRetry:
		delay := time.Second << min(job.Attempts, int32(6))
		delay = max(delay, result.RetryAfter)
		if sent.job.Scope == ScopeRound {
			delay = min(delay, roundQuietPeriod)
		}
		next = now.Add(min(delay, 5*time.Minute)).UnixNano()

	default:
		return errors.New("unknown provider outcome")
	}

	return finishSQLJob(ctx, q, job, done, next)
}

// finishSQLJob changes only completion fields; an ingress-owned newer
// generation/event/expiry remains intact throughout a provider request.
func finishSQLJob(ctx context.Context, q *sqlc.Queries, job sqlc.RelayJob, done,
	next int64) error {

	_, err := q.FinishJob(ctx, sqlc.FinishJobParams{
		ID: job.ID, ClaimToken: job.ClaimToken,
		DoneGeneration: done, NextAt: next,
	})

	return err
}

// pruneExpired makes bounded indexed progress on optional delivery and replay
// records. Expired subscriptions cascade to their local jobs and feed bindings.
func (s *SQLStore) pruneExpired(ctx context.Context, now time.Time) error {
	return s.database.Write(ctx, func(q *sqlc.Queries) error {
		if _, err := q.PruneConsumedChallenges(
			ctx, sqlc.PruneConsumedChallengesParams{
				ExpiresAt: now.Unix(),
				Limit:     256,
			},
		); err != nil {
			return err
		}
		if _, err := q.PruneExpiredJobs(
			ctx, sqlc.PruneExpiredJobsParams{
				ExpiresAt: now.UnixNano(),
				Limit:     256,
			},
		); err != nil {
			return err
		}
		if _, err := q.PruneExpiredDedup(
			ctx, sqlc.PruneExpiredDedupParams{
				ExpiresAt: now.Unix(),
				Limit:     256,
			},
		); err != nil {
			return err
		}
		expired, err := q.ListExpiredSubscriptions(
			ctx, sqlc.ListExpiredSubscriptionsParams{
				ExpiresAt: now.Unix(),
				Limit:     256,
			},
		)
		if err != nil {
			return err
		}
		for _, sub := range expired {
			if err := q.DeleteSubscription(
				ctx, sqlc.DeleteSubscriptionParams{
					ID:             sub.ID,
					InstallationID: sub.InstallationID,
				},
			); err != nil {
				return err
			}
		}

		return nil
	})
}
