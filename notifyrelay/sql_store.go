package notifyrelay

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/lightninglabs/wavelength/notifyrelay/relaydb"
	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
)

// SQLStore borrows a dedicated relay database. The caller joins all HTTP,
// ingress and provider owners before closing that database. It imports no
// legacy file and has no network work inside transaction retries.
type SQLStore struct {
	challengeAuthority
	database *relaydb.Database
	sources  map[string]*feedSource
}

// NewSQLStore restores persistent challenge authority. Concurrent initializers
// always use the committed key, including when another replica inserted it.
func NewSQLStore(ctx context.Context, database *relaydb.Database,
	configs ...SourceConfig) (*SQLStore, error) {

	if database == nil {
		return nil, relaydb.ErrUnavailable
	}
	var key [32]byte
	err := database.Write(ctx, func(q *sqlc.Queries) error {
		key = [32]byte{}
		var candidate [32]byte
		if _, err := rand.Read(candidate[:]); err != nil {
			return err
		}
		if _, err := q.InsertMetadata(ctx, sqlc.InsertMetadataParams{
			Key: "challenge_key", Value: candidate[:],
		}); err != nil {
			return err
		}
		persisted, err := q.GetMetadata(ctx, "challenge_key")
		if err != nil {
			return err
		}
		copy(key[:], persisted)

		return nil
	})
	if err != nil {
		return nil, err
	}

	store := &SQLStore{database: database,
		challengeAuthority: challengeAuthority{
			challengeKey: key,
		}}
	if err := store.restoreSources(ctx, configs); err != nil {
		return nil, err
	}

	return store, nil
}

// loadInstallation restores verifier evidence without exposing deleted keys.
func (s *SQLStore) loadInstallation(ctx context.Context, id string) (
	installation, error) {

	return loadSQLInstallation(ctx, s.database.Queries, id)
}

// loadSQLInstallation decodes a schema-bounded device record for its owner.
func loadSQLInstallation(ctx context.Context, q *sqlc.Queries,
	id string) (installation, error) {

	row, err := q.GetInstallation(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || err == nil && row.Deleted {
		return installation{}, ErrNotFound
	}
	if err != nil {
		return installation{}, err
	}
	var policy AppConfig
	if err := json.Unmarshal(row.Policy, &policy); err != nil {
		return installation{}, err
	}

	return installation{
		ID: row.ID, Policy: policy, PublicKey: row.PublicKey,
		Receipt: row.AttestReceipt,
		// Schema checks constrain the counter to uint32 and revision >
		// 0.
		Counter:       uint32(row.AssertionCounter),
		TokenRevision: uint64(row.TokenRevision),
		DeviceToken:   row.DeviceToken, TokenUpdated: row.TokenUpdated,
		NextWakeAt: row.NextWakeAt, NextAlertAt: row.NextAlertAt,
		NextRoundAt:    row.NextRoundAt,
		NextScheduleAt: row.NextScheduleAt,
		Disabled:       row.Disabled, Deleted: row.Deleted,
	}, nil
}

// saveSQLInstallation preserves the counter/revision SQL fences. A caller's
// stale mutation cannot overwrite a newer device token or assertion counter.
func saveSQLInstallation(ctx context.Context, q *sqlc.Queries,
	value installation) error {

	policy, err := json.Marshal(value.Policy)
	if err != nil {
		return err
	}
	rows, err := q.PutInstallation(ctx, sqlc.PutInstallationParams{
		ID: value.ID, Policy: policy, PublicKey: value.PublicKey,
		AttestReceipt: value.Receipt, AssertionCounter: int64(
			value.Counter,
		),
		DeviceToken:   value.DeviceToken,
		TokenRevision: int64(value.TokenRevision),
		TokenUpdated:  value.TokenUpdated,
		NextWakeAt:    value.NextWakeAt, NextAlertAt: value.NextAlertAt,
		NextRoundAt:    value.NextRoundAt,
		NextScheduleAt: value.NextScheduleAt,
		Disabled:       value.Disabled, Deleted: value.Deleted,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrReplay
	}

	return nil
}

// pruneChallenges commits bounded replay cleanup independently of a new
// admission, so rejected requests cannot indefinitely prevent expiry progress.
func (s *SQLStore) pruneChallenges(ctx context.Context, now time.Time) error {
	return s.database.Write(ctx, func(q *sqlc.Queries) error {
		_, err := q.PruneConsumedChallenges(ctx,
			sqlc.PruneConsumedChallengesParams{
				ExpiresAt: now.Unix(), Limit: 256,
			},
		)

		return err
	})
}

// consumeSQLChallenge joins a successful proof to its authorized mutation.
// Anonymous challenge issuance creates no row and cannot fill this budget.
func consumeSQLChallenge(ctx context.Context, q *sqlc.Queries,
	challenge Challenge, now time.Time) error {

	if challenge.ExpiresAt <= now.Unix() {
		return ErrReplay
	}
	count, err := q.CountConsumedChallenges(ctx)
	if err != nil {
		return err
	}
	if count >= maxConsumedChallenges {
		return ErrCapacity
	}
	rows, err := q.ConsumeChallenge(ctx, sqlc.ConsumeChallengeParams{
		ID: secretHash(challenge.ID), ExpiresAt: challenge.ExpiresAt,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrReplay
	}

	return nil
}

// enroll saves verified evidence once. Re-enrollment cannot reset assertion
// counters, including after deletion left a permanent installation tombstone.
func (s *SQLStore) enroll(ctx context.Context, challenge Challenge,
	value installation, now time.Time) error {

	if err := s.pruneChallenges(ctx, now); err != nil {
		return err
	}

	return s.database.Write(ctx, func(q *sqlc.Queries) error {
		_, err := q.GetInstallation(ctx, value.ID)
		if err == nil {
			return ErrReplay
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := consumeSQLChallenge(
			ctx, q, challenge, now,
		); err != nil {
			return err
		}

		return saveSQLInstallation(ctx, q, value)
	})
}

// applyManagement consumes a proof, advances the assertion counter and applies
// exactly one device action in one transaction. No capabilities are issued:
// the feed registration slice adds subscriber-signed source offers instead.
func (s *SQLStore) applyManagement(ctx context.Context, challenge Challenge,
	counter uint32, action ManagementPayload, now time.Time) (
	ManagementResult, error) {

	if err := s.pruneChallenges(ctx, now); err != nil {
		return ManagementResult{}, err
	}
	var result ManagementResult
	err := s.database.Write(ctx, func(q *sqlc.Queries) error {
		result = ManagementResult{}
		value, err := loadSQLInstallation(
			ctx, q, challenge.InstallationID,
		)
		if err != nil {
			return err
		}
		if value.Policy.ID != challenge.AppID ||
			counter <= value.Counter {
			return ErrReplay
		}
		if err := consumeSQLChallenge(
			ctx, q, challenge, now,
		); err != nil {
			return err
		}
		switch action.Action {
		case "rotate_token":
			value.DeviceToken = action.DeviceToken
			value.TokenRevision++
			value.TokenUpdated = now.UnixNano()
			value.Disabled = false

		case "subscribe":
			if value.Disabled {
				return ErrNotFound
			}
			result, err = subscribeSQL(
				ctx, q, value.ID, action, now,
			)
			if err != nil {
				return err
			}
			result.FeedOffers, err = s.feedOffers(
				ctx, q, result.SubscriptionID,
			)
			if err != nil {
				return err
			}

		case "bind_feed":
			result, err = s.bindFeedTx(
				ctx, q, value.ID, action.SubscriptionID,
				action.FeedBinding, now,
			)
			if err != nil {
				return err
			}

		case "revoke_subscription":
			owned, err := q.GetSubscription(
				ctx, action.SubscriptionID,
			)
			if errors.Is(err, sql.ErrNoRows) || err == nil &&
				owned.InstallationID != value.ID {

				// Hide ownership and finish the proof/counter
				// mutation.
				break
			}
			if err != nil {
				return err
			}
			if err := q.DeleteSubscription(
				ctx, sqlc.DeleteSubscriptionParams{
					ID:             owned.ID,
					InstallationID: value.ID,
				},
			); err != nil {
				return err
			}

		case "delete_installation":
			value.Deleted, value.Disabled = true, true
			value.DeviceToken = ""
			value.PublicKey = nil
			value.Receipt = nil
			if err := q.DeleteInstallationSubscriptions(
				ctx, value.ID,
			); err != nil {
				return err
			}

		default:
			return errors.New("unsupported installation action")
		}
		value.Counter = counter

		return saveSQLInstallation(ctx, q, value)
	})

	return result, err
}

// subscribeSQL retains one publisher subscription ID per app-owned route.
// Presentation changes discard only old payment jobs, preserving dedup
// evidence.
func subscribeSQL(ctx context.Context, q *sqlc.Queries, owner string,
	action ManagementPayload, now time.Time) (ManagementResult, error) {

	sub, err := q.GetSubscriptionByRoute(
		ctx, sqlc.GetSubscriptionByRouteParams{
			InstallationID: owner,
			RouteID:        action.RouteID,
		},
	)
	if errors.Is(err, sql.ErrNoRows) {
		count, err := q.CountInstallationSubscriptions(ctx, owner)
		if err != nil {
			return ManagementResult{}, err
		}
		if count >= maxSubscriptions {
			return ManagementResult{}, ErrCapacity
		}
		id, err := randomSecret(24)
		if err != nil {
			return ManagementResult{}, err
		}
		sub = sqlc.RelaySubscription{
			ID: id, InstallationID: owner, RouteID: action.RouteID,
		}
	} else if err != nil {
		return ManagementResult{}, err
	}
	presentation := action.Presentation.normalized()
	if !action.EnableAlerts || sub.Presentation != string(presentation) {
		if err := q.DeleteScopeJob(ctx, sqlc.DeleteScopeJobParams{
			SubscriptionID: sub.ID, Scope: string(ScopeAlert),
		}); err != nil {
			return ManagementResult{}, err
		}
	}
	expiry := now.Add(subscriptionLifetime - 5*time.Minute).Unix()
	rows, err := q.PutSubscription(ctx, sqlc.PutSubscriptionParams{
		ID: sub.ID, InstallationID: owner, RouteID: action.RouteID,
		AlertsEnabled: action.EnableAlerts, Presentation: string(
			presentation,
		),
		ExpiresAt: expiry, Revision: sub.Revision + 1,
	})
	if err != nil {
		return ManagementResult{}, err
	}
	if rows != 1 {
		return ManagementResult{}, ErrReplay
	}

	return ManagementResult{
		SubscriptionID: sub.ID,
		ExpiresAt:      expiry,
		Presentation:   presentation,
	}, nil
}
