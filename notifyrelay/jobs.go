package notifyrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	// Successful silent pushes observe Apple's low-frequency background
	// delivery guidance. Alerts have an independent per-installation
	// budget.
	wakeQuietPeriod  = 20 * time.Minute
	alertQuietPeriod = time.Minute
)

// Outcome describes a provider result without exposing credentials or tokens.
type Outcome uint8

const (
	// OutcomeRetry retains the same durable job after a temporary failure.
	OutcomeRetry Outcome = iota
	// OutcomeAccepted means APNs accepted the request, not that it
	// displayed.
	OutcomeAccepted
	// OutcomeInvalidToken permits revision-fenced installation
	// invalidation.
	OutcomeInvalidToken
	// OutcomeRejected retires a malformed request that retry cannot repair.
	OutcomeRejected
)

// ProviderResult separates permanent token invalidation from transient errors.
// InvalidAt is APNs' invalidation timestamp when one was supplied.
type ProviderResult struct {
	Outcome    Outcome
	InvalidAt  time.Time
	RetryAfter time.Duration
}

// Delivery is a frozen provider request. DeviceToken is private routing data;
// String deliberately excludes it, and JSON serialization never emits it.
type Delivery struct {
	DeviceToken string    `json:"-"`
	Topic       string    `json:"topic"`
	Environment string    `json:"environment"`
	Scope       Scope     `json:"scope"`
	RouteID     string    `json:"route_id"`
	EventHash   string    `json:"event_hash"`
	CollapseID  string    `json:"collapse_id"`
	ExpiresAt   time.Time `json:"expires_at"`

	job           relayJob
	tokenRevision uint64
	policy        AppConfig
}

// String makes routine diagnostics safe even when a delivery is formatted.
func (d Delivery) String() string {
	return fmt.Sprintf("notification delivery (%s)", d.Scope)
}

// Provider sends only a server-constructed, fixed-template delivery. Errors
// must not contain device tokens, provider JWTs or request URLs containing
// them.
type Provider interface {
	// Send observes its context and returns APNs acceptance separately from
	// eventual device delivery. Implementations must not retarget a
	// delivery.
	Send(context.Context, Delivery) (ProviderResult, error)
}

// claim freezes one eligible job and its current token revision atomically.
// Expired/revoked jobs are removed before any provider call can occur.
func (s *Store) claim(ctx context.Context, now time.Time) (*Delivery, error) {
	claimToken, err := randomSecret(16)
	if err != nil {
		return nil, err
	}
	var result *Delivery
	err = s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		cursor := tx.Bucket([]byte(jobsBucket)).Cursor()
		key, raw := cursor.First()
		for ; key != nil; key, raw = cursor.Next() {
			var job relayJob
			if err := json.Unmarshal(raw, &job); err != nil {
				return err
			}
			sub, subErr := readRecord[subscription](
				tx, subscriptionsBucket, job.SubscriptionID,
			)
			owner, ownerErr := readRecord[installation](
				tx, installationsBucket, sub.InstallationID,
			)
			if subErr != nil && !errors.Is(subErr, ErrNotFound) {
				return subErr
			}
			if ownerErr != nil &&
				!errors.Is(ownerErr, ErrNotFound) {
				return ownerErr
			}
			if subErr != nil || ownerErr != nil ||
				owner.Disabled || owner.Deleted ||
				(job.Scope == ScopeAlert &&
					sub.AlertHash == "") ||
				sub.ExpiresAt <= now.Unix() ||
				job.ExpiresAt <= now.UnixNano() {

				if err := cursor.Delete(); err != nil {
					return err
				}

				continue
			}
			next := owner.NextWakeAt
			if job.Scope == ScopeAlert {
				next = owner.NextAlertAt
			}
			if job.NextAt > now.UnixNano() ||
				job.LeaseUntil > now.UnixNano() ||
				next > now.UnixNano() {

				continue
			}
			job.ClaimToken = claimToken
			job.LeaseUntil = now.Add(claimLifetime).UnixNano()
			job.Attempts++
			if err := putRecord(
				tx, jobsBucket, job.ID, job,
			); err != nil {
				return err
			}
			result = &Delivery{
				DeviceToken:   owner.DeviceToken,
				Topic:         owner.Policy.Topic,
				Environment:   owner.Policy.APNSEnvironment,
				Scope:         job.Scope,
				RouteID:       sub.RouteID,
				EventHash:     job.EventHash,
				CollapseID:    secretHash(job.ID),
				ExpiresAt:     time.Unix(0, job.ExpiresAt),
				job:           job,
				tokenRevision: owner.TokenRevision,
				policy:        owner.Policy,
			}

			return nil
		}

		return nil
	})

	return result, err
}

// complete fences an old claim before applying provider results. A new event
// or token revision admitted during the send remains eligible for delivery.
func (s *Store) complete(ctx context.Context, sent Delivery,
	result ProviderResult, now time.Time) error {

	return s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		job, err := readRecord[relayJob](tx, jobsBucket, sent.job.ID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if job.ClaimToken != sent.job.ClaimToken {
			return nil
		}
		sub, err := readRecord[subscription](
			tx, subscriptionsBucket, job.SubscriptionID,
		)
		if errors.Is(err, ErrNotFound) {
			return tx.Bucket([]byte(jobsBucket)).Delete(
				[]byte(job.ID),
			)
		}
		if err != nil {
			return err
		}
		owner, err := readRecord[installation](
			tx, installationsBucket, sub.InstallationID,
		)
		if err != nil {
			return err
		}
		if result.Outcome == OutcomeInvalidToken {
			current := owner.TokenRevision == sent.tokenRevision
			recent := result.InvalidAt.IsZero() ||
				!result.InvalidAt.Before(
					time.Unix(0, owner.TokenUpdated),
				)
			if current && recent {
				owner.Disabled = true
				if err := putRecord(
					tx, installationsBucket, owner.ID,
					owner,
				); err != nil {
					return err
				}

				return tx.Bucket([]byte(jobsBucket)).Delete(
					[]byte(job.ID),
				)
			}
			// A response for a replaced revision leaves work for
			// the new token. A current-revision rejection with an
			// older invalidation timestamp retires this job without
			// disabling an installation whose registration is more
			// recent.
			result.Outcome = OutcomeRetry
			if current {
				result.Outcome = OutcomeRejected
			}
		}
		switch result.Outcome {
		case OutcomeAccepted, OutcomeRejected:
			job.DoneGeneration = max(
				job.DoneGeneration, sent.job.Generation,
			)
			if result.Outcome == OutcomeAccepted {
				if job.Scope == ScopeWake {
					owner.NextWakeAt = now.
						Add(wakeQuietPeriod).
						UnixNano()
				} else {
					owner.NextAlertAt = now.
						Add(alertQuietPeriod).
						UnixNano()
				}
				if err := putRecord(
					tx, installationsBucket, owner.ID,
					owner,
				); err != nil {
					return err
				}
			}
			if job.Generation == job.DoneGeneration {
				return tx.Bucket([]byte(jobsBucket)).Delete(
					[]byte(job.ID),
				)
			}
			job.NextAt = now.UnixNano()

		case OutcomeRetry:
			delay := time.Second << min(job.Attempts, uint32(6))
			delay = max(delay, result.RetryAfter)
			job.NextAt = now.
				Add(min(delay, 5*time.Minute)).
				UnixNano()

		default:
			return errors.New("unknown provider outcome")
		}
		job.ClaimToken = ""
		job.LeaseUntil = 0

		return putRecord(tx, jobsBucket, job.ID, job)
	})
}

// maintenanceRecords visits at most 256 copied rows per bucket and persists
// the scan cursor, so live rows cannot starve expired rows later in key order.
func maintenanceRecords(tx *bolt.Tx, bucketName string,
	visit func([]byte, []byte) error) error {

	meta := tx.Bucket([]byte(metadataBucket))
	bookmarkKey := []byte("gc/" + bucketName)
	bookmark := meta.Get(bookmarkKey)
	cursor := tx.Bucket([]byte(bucketName)).Cursor()
	key, raw := cursor.First()
	if len(bookmark) != 0 {
		key, raw = cursor.Seek(bookmark)
		if bytes.Equal(key, bookmark) {
			key, raw = cursor.Next()
		}
	}
	var records [][2][]byte
	var last []byte
	for key != nil && len(records) < 256 {
		last = bytes.Clone(key)
		records = append(records, [2][]byte{last, bytes.Clone(raw)})
		key, raw = cursor.Next()
	}
	if key == nil {
		if err := meta.Delete(bookmarkKey); err != nil {
			return err
		}
	} else if err := meta.Put(bookmarkKey, last); err != nil {
		return err
	}
	// Mutation follows the scan; deleting a record cannot invalidate its
	// cursor.
	for _, record := range records {
		if err := visit(record[0], record[1]); err != nil {
			return err
		}
	}

	return nil
}

// pruneExpired bounds examined rows and removes only expired replay authority.
func (s *Store) pruneExpired(ctx context.Context, now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, name := range []string{dedupBucket, challengesBucket} {
			err := maintenanceRecords(
				tx, name,
				func(key, raw []byte) error {
					var expiry int64
					if err := json.Unmarshal(
						raw, &expiry,
					); err != nil {
						return err
					}
					if expiry <= now.Unix() {
						return tx.
							Bucket([]byte(name)).
							Delete(key)
					}

					return nil
				},
			)
			if err != nil {
				return err
			}
		}

		return maintenanceRecords(
			tx, subscriptionsBucket,
			func(_, raw []byte) error {
				var value subscription
				if err := json.Unmarshal(
					raw, &value,
				); err != nil {
					return err
				}
				if value.ExpiresAt <= now.Unix() {
					return revokeSubscription(
						tx, value.InstallationID,
						value.ID,
					)
				}

				return nil
			},
		)
	})
}
