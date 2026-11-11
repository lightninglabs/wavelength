package notifyrelay

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/notifyrelay/relaydb"
	"github.com/stretchr/testify/require"
)

// sqlAuthHarness isolates store transactions from verifier policy. The HTTP
// integration suite separately proves real App Attest transcript verification.
type sqlAuthHarness struct {
	store    *SQLStore
	database *relaydb.Database
	open     func() *relaydb.Database
	now      time.Time
}

// newSQLAuthHarness enrolls one verified installation into a dedicated store.
func newSQLAuthHarness(t *testing.T) *sqlAuthHarness {
	t.Helper()
	open := sqlTestOpener(t)
	database := open()
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	store, err := NewSQLStore(t.Context(), database)
	require.NoError(t, err)
	h := &sqlAuthHarness{
		store:    store,
		database: database,
		open:     open,
		now:      time.Now(),
	}
	challenge, err := store.NewChallenge(t.Context(), ChallengeRequest{
		AppID: "app", Operation: OperationEnroll,
	}, h.now)
	require.NoError(t, err)
	require.NoError(
		t,
		store.enroll(
			t.Context(), challenge, installation{
				ID:            "device",
				Policy:        AppConfig{ID: "app"},
				PublicKey:     []byte{1},
				DeviceToken:   "token",
				TokenRevision: 1,
			},
			h.now,
		),
	)

	return h
}

// challenge issues a new nonce bound to the enrolled device and management.
func (h *sqlAuthHarness) challenge(t *testing.T) Challenge {
	t.Helper()
	challenge, err := h.store.NewChallenge(t.Context(), ChallengeRequest{
		AppID: "app", Operation: OperationManage,
		InstallationID: "device",
	}, h.now)
	require.NoError(t, err)

	return challenge
}

// TestSQLManagementRace consumes one valid proof once across concurrent
// transaction owners, and retains stable subscription IDs through renewal.
func TestSQLManagementRace(t *testing.T) {
	h := newSQLAuthHarness(t)
	challenge := h.challenge(t)
	action := ManagementPayload{Action: "subscribe", RouteID: "wallet"}
	var writers sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		writers.Go(func() {
			_, err := h.store.applyManagement(
				t.Context(), challenge, 1, action, h.now,
			)
			results <- err
		})
	}
	writers.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, ErrReplay)
		}
	}
	require.Equal(t, 1, successes)
	subscriptions, err := h.database.Queries.ListInstallationSubscriptions(
		t.Context(),
		"device",
	)
	require.NoError(t, err)
	require.Len(t, subscriptions, 1)
	first := subscriptions[0]
	renewed, err := h.store.applyManagement(
		t.Context(), h.challenge(t), 2, action, h.now,
	)
	require.NoError(t, err)
	require.Equal(t, first.ID, renewed.SubscriptionID)
	require.Less(
		t, renewed.ExpiresAt, h.now.Add(subscriptionLifetime).Unix(),
	)
	require.Equal(t, 24, decodedSecretLength(t, renewed.SubscriptionID))
}

// decodedSecretLength verifies publisher identifiers have random-byte encoding
// suitable for the independent-publisher grant namespace.
func decodedSecretLength(t *testing.T, value string) int {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	require.NoError(t, err)

	return len(decoded)
}

// TestSQLManagementRollback proves a rejected ownership mutation retains the
// nonce/counter for a valid retry, and deletion cannot reset an installation.
func TestSQLManagementRollback(t *testing.T) {
	h := sourceHarness(t, testSourceConfig(t, "producer"))
	challenge := h.challenge(t)
	_, err := h.store.applyManagement(
		t.Context(), challenge, 1, ManagementPayload{
			Action:         "bind_feed",
			SubscriptionID: "foreign",
			FeedBinding: &FeedBindingRequest{
				SourceID: "producer",
				FeedToken: base64.RawURLEncoding.EncodeToString(
					bytes.Repeat(
						[]byte{1}, 32,
					),
				),
				EventClasses:  1,
				ExpiresAtUnix: h.now.Add(time.Minute).Unix(),
			},
		},
		h.now,
	)
	require.ErrorIs(t, err, ErrNotFound)
	owner, err := h.store.loadInstallation(t.Context(), "device")
	require.NoError(t, err)
	require.Zero(t, owner.Counter)
	_, err = h.database.Queries.GetConsumedChallenge(
		t.Context(), secretHash(challenge.ID),
	)
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, err = h.store.applyManagement(
		t.Context(), challenge, 1, ManagementPayload{
			Action:      "rotate_token",
			DeviceToken: "new-token",
		},
		h.now,
	)
	require.NoError(t, err)
	owner, err = h.store.loadInstallation(t.Context(), "device")
	require.NoError(t, err)
	require.EqualValues(t, 1, owner.Counter)
	require.EqualValues(t, 2, owner.TokenRevision)
	require.Equal(t, "new-token", owner.DeviceToken)
	_, err = h.store.applyManagement(
		t.Context(), h.challenge(t), 2, ManagementPayload{
			Action: "delete_installation",
		},
		h.now,
	)
	require.NoError(t, err)
	_, err = h.store.loadInstallation(t.Context(), "device")
	require.ErrorIs(t, err, ErrNotFound)
	owner.Counter = 0
	owner.TokenRevision = 1
	require.ErrorIs(
		t,
		h.store.enroll(
			t.Context(), h.challenge(t), owner, h.now,
		),
		ErrReplay,
	)
	retained, err := h.database.Queries.GetInstallation(
		t.Context(),
		"device",
	)
	require.NoError(t, err)
	require.True(t, retained.Deleted)
	require.Empty(t, retained.PublicKey)
	require.Empty(t, retained.AttestReceipt)
}

// TestSQLAuthorityRestart restores the committed challenge key after a real
// pool restart rather than losing issued nonce authority on process restart.
func TestSQLAuthorityRestart(t *testing.T) {
	open := sqlTestOpener(t)
	database := open()
	store, err := NewSQLStore(t.Context(), database)
	require.NoError(t, err)
	challenge, err := store.NewChallenge(t.Context(), ChallengeRequest{
		AppID: "app", Operation: OperationEnroll,
	}, time.Now())
	require.NoError(t, err)
	key := bytes.Clone(store.challengeKey[:])
	require.NoError(t, database.Close())
	database = open()
	defer func() {
		require.NoError(t, database.Close())
	}()
	store, err = NewSQLStore(t.Context(), database)
	require.NoError(t, err)
	require.Equal(t, key, store.challengeKey[:])
	restored, err := store.loadChallenge(
		context.Background(), challenge.ID, time.Now(),
	)
	require.NoError(t, err)
	require.Equal(t, challenge, restored)
	count, err := database.Queries.CountConsumedChallenges(t.Context())
	require.NoError(t, err)
	require.Zero(t, count, "anonymous issuance allocates no replay rows")
}
