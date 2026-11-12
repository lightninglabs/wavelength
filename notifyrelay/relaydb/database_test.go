package relaydb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
	"github.com/stretchr/testify/require"
)

// TestRelayDatabaseRestart checks byte fidelity, immutable keys, and monotonic
// installation/route state on both engines, including a real pool restart.
func TestRelayDatabaseRestart(t *testing.T) {
	open := testOpener(t)
	store := open()
	ctx := t.Context()
	key := bytes.Repeat([]byte{0x80}, 32)
	key[0] = 0
	installation := sqlc.PutInstallationParams{
		ID: "device", Policy: []byte{
			0,
			255,
		}, PublicKey: []byte{
			1,
			0,
		},
		AttestReceipt: []byte{}, AssertionCounter: 4,
		DeviceToken: "token", TokenRevision: 2, TokenUpdated: 100,
	}
	err := store.Write(ctx, func(q *sqlc.Queries) error {
		if _, err := q.InsertMetadata(ctx, sqlc.InsertMetadataParams{
			Key: "challenge", Value: key,
		}); err != nil {
			return err
		}
		_, err := q.PutInstallation(ctx, installation)

		return err
	})
	require.NoError(t, err)
	source := sqlc.InsertSourceParams{
		ID: "source", SourcePubkey: bytes.Repeat([]byte{2}, 33),
		SubscriberKey: key,
	}
	inserted, err := store.Queries.InsertSource(ctx, source)
	require.NoError(t, err)
	require.EqualValues(t, 1, inserted)
	source.SubscriberKey = bytes.Repeat([]byte{3}, 32)
	source.SourcePubkey = bytes.Repeat([]byte{3}, 33)
	inserted, err = store.Queries.InsertSource(ctx, source)
	require.NoError(t, err)
	require.Zero(
		t, inserted,
		"caller must restore and verify the retained source",
	)
	route := sqlc.PutSubscriptionParams{
		ID: "subscription", InstallationID: "device", RouteID: "route",
		Presentation: "generic", ExpiresAt: 1000, Revision: 3,
	}
	rows, err := store.Queries.PutSubscription(ctx, route)
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)

	// Old counters and token revisions cannot roll state back. A route
	// cannot be reassigned by reusing its publisher-generated subscription
	// ID.
	installation.AssertionCounter--
	rows, err = store.Queries.PutInstallation(ctx, installation)
	require.NoError(t, err)
	require.Zero(t, rows)
	installation.AssertionCounter += 2
	installation.TokenRevision--
	rows, err = store.Queries.PutInstallation(ctx, installation)
	require.NoError(t, err)
	require.Zero(t, rows)
	route.RouteID = "other-route"
	rows, err = store.Queries.PutSubscription(ctx, route)
	require.NoError(t, err)
	require.Zero(t, rows)
	require.NoError(t, store.Close())
	store = open()
	defer func() {
		require.NoError(t, store.Close())
	}()
	actualKey, err := store.Queries.GetMetadata(ctx, "challenge")
	require.NoError(t, err)
	require.Equal(t, key, actualKey)
	actual, err := store.Queries.GetInstallation(ctx, "device")
	require.NoError(t, err)
	require.Equal(t, []byte{0, 255}, actual.Policy)
	require.EqualValues(t, 4, actual.AssertionCounter)
	require.EqualValues(t, 2, actual.TokenRevision)
	retained, err := store.Queries.GetSource(ctx, "source")
	require.NoError(t, err)
	require.Equal(t, key, retained.SubscriberKey)
	require.Equal(t, bytes.Repeat([]byte{2}, 33), retained.SourcePubkey)
	retainedRoute, err := store.Queries.GetSubscription(ctx, "subscription")
	require.NoError(t, err)
	require.Equal(t, "route", retainedRoute.RouteID)
}

// TestRelayDatabaseAtomicity proves nonce consumption rolls back with failed
// device mutations and concurrent serializable writers preserve every counter.
func TestRelayDatabaseAtomicity(t *testing.T) {
	store := testOpener(t)()
	defer func() {
		require.NoError(t, store.Close())
	}()
	ctx := t.Context()
	installation := sqlc.PutInstallationParams{
		ID: "device", Policy: []byte("{}"), PublicKey: []byte{
			1,
		},
		AttestReceipt: []byte{}, DeviceToken: "token", TokenRevision: 1,
	}
	_, err := store.Queries.PutInstallation(ctx, installation)
	require.NoError(t, err)
	injected := errors.New("transaction interrupted")
	err = store.Write(ctx, func(q *sqlc.Queries) error {
		if _, err := q.ConsumeChallenge(
			ctx, sqlc.ConsumeChallengeParams{
				ID:        "nonce",
				ExpiresAt: 1000,
			},
		); err != nil {
			return err
		}
		installation.AssertionCounter = 1
		if _, err := q.PutInstallation(ctx, installation); err != nil {
			return err
		}

		return injected
	})
	require.ErrorIs(t, err, injected)
	_, err = store.Queries.GetConsumedChallenge(ctx, "nonce")
	require.ErrorIs(t, err, sql.ErrNoRows)
	actual, err := store.Queries.GetInstallation(ctx, "device")
	require.NoError(t, err)
	require.Zero(t, actual.AssertionCounter)

	// The production retry closure contains only local SQL. A serialization
	// conflict re-reads the counter, rather than replaying a stale
	// increment.
	var writers sync.WaitGroup
	failures := make(chan error, 2)
	for range 2 {
		writers.Go(func() {
			for range 10 {
				err := incrementCounter(ctx, store)
				if err != nil {
					failures <- err

					return
				}
			}
		})
	}
	writers.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	actual, err = store.Queries.GetInstallation(ctx, "device")
	require.NoError(t, err)
	require.EqualValues(t, 20, actual.AssertionCounter)
	rows, err := store.Queries.ConsumeChallenge(
		ctx, sqlc.ConsumeChallengeParams{
			ID:        "nonce",
			ExpiresAt: 1000,
		},
	)
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	rows, err = store.Queries.ConsumeChallenge(
		ctx, sqlc.ConsumeChallengeParams{
			ID:        "nonce",
			ExpiresAt: 1000,
		},
	)
	require.NoError(t, err)
	require.Zero(t, rows)
}

// incrementCounter exercises the same read/check/write transaction shape as
// installation assertions, without a network or verifier side effect.
func incrementCounter(ctx context.Context, store *Database) error {
	return store.Write(ctx, func(q *sqlc.Queries) error {
		current, err := q.GetInstallation(ctx, "device")
		if err != nil {
			return err
		}
		_, err = q.PutInstallation(ctx, sqlc.PutInstallationParams{
			ID: current.ID, Policy: current.Policy,
			PublicKey:        current.PublicKey,
			AttestReceipt:    current.AttestReceipt,
			AssertionCounter: current.AssertionCounter + 1,
			DeviceToken:      current.DeviceToken,
			TokenRevision:    current.TokenRevision,
		})

		return err
	})
}

// TestRelayStartupErrorRedaction preserves useful migration diagnosis without
// returning a raw driver error that could contain publisher credentials.
func TestRelayStartupErrorRedaction(t *testing.T) {
	_, err := OpenPostgres(t.Context(), "postgres://secret:secret@%invalid")
	require.ErrorIs(t, err, ErrUnavailable)
	require.NotContains(t, err.Error(), "secret")
	err = unavailable("schema migration", migrate.ErrDirty{Version: 1})
	require.ErrorIs(t, err, ErrUnavailable)
	require.Contains(t, err.Error(), "dirty version 1")
}
