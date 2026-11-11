package notifyrelay

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notification"
	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
	"github.com/stretchr/testify/require"
)

// testSourceConfig creates public producer identity policy without any live
// endpoint, device credential or wallet key.
func testSourceConfig(t *testing.T, id string) SourceConfig {
	t.Helper()
	source, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return SourceConfig{ID: id, Role: "operator",
		Endpoint: "producer.example:443",
		PublicKey: hex.EncodeToString(
			source.PubKey().SerializeCompressed(),
		), EventClasses: 3}
}

// sourceHarness restores configured subscriber authority alongside the normal
// verified device-management fixture.
func sourceHarness(t *testing.T, configs ...SourceConfig) *sqlAuthHarness {
	t.Helper()
	h := newSQLAuthHarness(t)
	store, err := NewSQLStore(t.Context(), h.database, configs...)
	require.NoError(t, err)
	h.store = store

	return h
}

// TestFeedSourceKeysRestoreAndSeparate prevents cross-producer reuse of the
// existing static mailbox auth proof and source-ID identity replacement.
func TestFeedSourceKeysRestoreAndSeparate(t *testing.T) {
	first := testSourceConfig(t, "first")
	second := testSourceConfig(t, "second")
	h := sourceHarness(t, first, second)
	a := h.store.sources[first.ID]
	b := h.store.sources[second.ID]
	require.NotEqual(t, a.mailboxID, b.mailboxID)
	peerDB := h.open()
	defer func() {
		require.NoError(t, peerDB.Close())
	}()
	peer, err := NewSQLStore(t.Context(), peerDB, second, first)
	require.NoError(t, err)
	require.Equal(t, a.mailboxID, peer.sources[first.ID].mailboxID)
	require.Equal(t, b.mailboxID, peer.sources[second.ID].mailboxID)
	require.Equal(
		t, a.subscriber.Serialize(),
		peer.sources[first.ID].subscriber.Serialize(),
	)
	replacement := testSourceConfig(t, "first")
	_, err = NewSQLStore(t.Context(), peerDB, replacement)
	require.Error(
		t, err, "one producer cannot inherit another producer's "+
			"subscriber proof",
	)
	saved, err := h.database.Queries.GetSource(t.Context(), first.ID)
	require.NoError(t, err)
	require.Equal(t, a.public.SerializeCompressed(), saved.SourcePubkey)
}

// TestFeedOffersHavePublisherConsent binds the random subscription ID rather
// than an app-local route, and proves every offer against the shared verifier.
func TestFeedOffersHavePublisherConsent(t *testing.T) {
	first := testSourceConfig(t, "first")
	second := testSourceConfig(t, "second")
	h := sourceHarness(t, second, first)
	result, err := h.store.applyManagement(
		t.Context(), h.challenge(t), 1, ManagementPayload{
			Action:       "subscribe",
			RouteID:      "local-wallet",
			EnableAlerts: true,
		},
		h.now,
	)
	require.NoError(t, err)
	require.Len(t, result.FeedOffers, 2)
	require.Equal(t, "first", result.FeedOffers[0].SourceID)
	for _, offer := range result.FeedOffers {
		require.Equal(t, result.SubscriptionID, offer.GrantID)
		require.NotEqual(t, "local-wallet", offer.GrantID)
		public, err := hex.DecodeString(offer.SourcePubkey)
		require.NoError(t, err)
		subscriber, err := hex.DecodeString(offer.SubscriberPubkey)
		require.NoError(t, err)
		signature, err := base64.RawURLEncoding.DecodeString(
			offer.SubscriberSignature,
		)
		require.NoError(t, err)
		mutation := &pb.FeedGrantMutation{
			Action:              pb.FeedGrantMutation_ACTION_UPSERT,
			GrantId:             offer.GrantID,
			SubscriberPubkey:    subscriber,
			SubscriberSignature: signature,
			EventClasses:        offer.EventClasses,
			ExpiresAtUnix:       offer.ExpiresAtUnix,
		}
		require.NoError(
			t,
			notification.VerifyFeedRouteConsent(public, mutation),
		)
		require.NoError(
			t, notification.ValidateFeedMutation(mutation, h.now),
		)
	}
	renewed, err := h.store.applyManagement(
		t.Context(), h.challenge(t), 2, ManagementPayload{
			Action:  "subscribe",
			RouteID: "local-wallet",
		},
		h.now,
	)
	require.NoError(t, err)
	for _, offer := range renewed.FeedOffers {
		require.Equal(t, notification.FeedClassWake, offer.EventClasses)
	}
}

// bindTestFeed submits correlation through the same counter/nonce transaction
// as authenticated device management.
func (h *sqlAuthHarness) bindTestFeed(t *testing.T, id string,
	request FeedBindingRequest) error {

	t.Helper()
	owner, err := h.store.loadInstallation(t.Context(), "device")
	require.NoError(t, err)
	_, err = h.store.applyManagement(
		t.Context(), h.challenge(t), owner.Counter+1,
		ManagementPayload{
			Action:         "bind_feed",
			SubscriptionID: id,
			FeedBinding:    &request,
		},
		h.now,
	)

	return err
}

// TestFeedBindingCannotPoisonAnotherRoute keeps equal non-authorizing tokens
// in separate local route bindings and rejects a device's foreign route ID.
func TestFeedBindingCannotPoisonAnotherRoute(t *testing.T) {
	source := testSourceConfig(t, "producer")
	h := sourceHarness(t, source)
	first := h.sqlSubscription(t, "first")
	second := h.sqlSubscription(t, "second")
	token := bytes.Repeat([]byte{3}, 32)
	request := FeedBindingRequest{
		SourceID: source.ID,
		FeedToken: base64.RawURLEncoding.EncodeToString(
			token,
		),
		EventClasses: 3, ExpiresAtUnix: h.now.Add(time.Hour).Unix(),
	}
	require.NoError(t, h.bindTestFeed(t, first, request))
	require.NoError(t, h.bindTestFeed(t, second, request))
	for _, id := range []string{first, second} {
		binding, err := h.database.Queries.GetFeedBinding(
			t.Context(), sqlc.GetFeedBindingParams{
				SourceID:       source.ID,
				SubscriptionID: id,
			},
		)
		require.NoError(t, err)
		require.Equal(t, id, binding.SubscriptionID)
		require.Equal(t, token, binding.FeedToken)
	}
	challenge, err := h.store.NewChallenge(
		t.Context(), ChallengeRequest{
			AppID:     "app",
			Operation: OperationEnroll,
		},
		h.now,
	)
	require.NoError(t, err)
	require.NoError(
		t,
		h.store.enroll(
			t.Context(), challenge, installation{
				ID:            "other",
				Policy:        AppConfig{ID: "app"},
				PublicKey:     []byte{2},
				DeviceToken:   "other",
				TokenRevision: 1,
			},
			h.now,
		),
	)
	challenge, err = h.store.NewChallenge(
		t.Context(), ChallengeRequest{
			AppID:          "app",
			Operation:      OperationManage,
			InstallationID: "other",
		},
		h.now,
	)
	require.NoError(t, err)
	other, err := h.store.applyManagement(
		t.Context(), challenge, 1, ManagementPayload{
			Action:       "subscribe",
			RouteID:      "target",
			EnableAlerts: true,
		},
		h.now,
	)
	require.NoError(t, err)
	before, err := h.store.loadInstallation(t.Context(), "device")
	require.NoError(t, err)
	require.ErrorIs(
		t, h.bindTestFeed(t, other.SubscriptionID, request),
		ErrNotFound,
	)
	after, err := h.store.loadInstallation(t.Context(), "device")
	require.NoError(t, err)
	require.Equal(t, before.Counter, after.Counter)
}

// TestFeedBindingPolicyAndReplay rejects widened classes/expiry and preserves
// counter and nonce on failure. Renewal of one token retains ingestion
// progress.
func TestFeedBindingPolicyAndReplay(t *testing.T) {
	source := testSourceConfig(t, "producer")
	source.EventClasses = notification.FeedClassWake
	h := sourceHarness(t, source)
	id := h.sqlSubscription(t, "wallet")
	request := FeedBindingRequest{SourceID: source.ID,
		FeedToken: base64.RawURLEncoding.EncodeToString(
			bytes.Repeat(
				[]byte{8}, 32,
			),
		),
		EventClasses: 3, ExpiresAtUnix: h.now.Add(time.Hour).Unix()}
	require.ErrorIs(t, h.bindTestFeed(t, id, request), ErrNotFound)
	request.EventClasses = notification.FeedClassWake
	require.NoError(t, h.bindTestFeed(t, id, request))
	raw, err := base64.RawURLEncoding.DecodeString(request.FeedToken)
	require.NoError(t, err)
	rows, err := h.database.Queries.AdvanceFeedBinding(
		t.Context(), sqlc.AdvanceFeedBindingParams{
			SourceID:       source.ID,
			SubscriptionID: id,
			FeedToken:      raw,
			Generation:     7,
			NowUnix:        h.now.Unix(),
		},
	)
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	require.NoError(t, h.bindTestFeed(t, id, request))
	bound, err := h.database.Queries.GetFeedBinding(
		t.Context(), sqlc.GetFeedBindingParams{
			SourceID:       source.ID,
			SubscriptionID: id,
		},
	)
	require.NoError(t, err)
	require.EqualValues(t, 7, bound.LastGeneration)
	request.FeedToken = base64.RawURLEncoding.EncodeToString(
		bytes.Repeat(
			[]byte{9}, 32,
		),
	)
	require.NoError(t, h.bindTestFeed(t, id, request))
	bound, err = h.database.Queries.GetFeedBinding(
		t.Context(), sqlc.GetFeedBindingParams{
			SourceID:       source.ID,
			SubscriptionID: id,
		},
	)
	require.NoError(t, err)
	require.Zero(t, bound.LastGeneration)
	request.ExpiresAtUnix = h.now.Add(subscriptionLifetime).Unix()
	require.ErrorIs(t, h.bindTestFeed(t, id, request), ErrNotFound)
	request.ExpiresAtUnix = h.now.Add(time.Hour).Unix()
	require.ErrorIs(t, h.bindTestFeed(t, "missing", request), ErrNotFound)
	request.SourceID = "not-configured"
	require.ErrorIs(t, h.bindTestFeed(t, id, request), ErrNotFound)
}

// TestFeedBindingRevocationCascades proves both database backends remove the
// binding when verified management deletes its subscription or installation.
func TestFeedBindingRevocationCascades(t *testing.T) {
	for _, action := range []string{
		"revoke_subscription",
		"delete_installation",
	} {
		t.Run(action, func(t *testing.T) {
			source := testSourceConfig(t, "source")
			h := sourceHarness(t, source)
			id := h.sqlSubscription(t, "wallet")
			request := FeedBindingRequest{
				SourceID: source.ID,
				FeedToken: base64.RawURLEncoding.EncodeToString(
					bytes.Repeat(
						[]byte{7}, 32,
					),
				),
				EventClasses:  1,
				ExpiresAtUnix: h.now.Add(time.Hour).Unix(),
			}
			require.NoError(t, h.bindTestFeed(t, id, request))
			owner, err := h.store.loadInstallation(
				t.Context(),
				"device",
			)
			require.NoError(t, err)
			payload := ManagementPayload{Action: action}
			if action == "revoke_subscription" {
				payload.SubscriptionID = id
			}
			_, err = h.store.applyManagement(
				t.Context(), h.challenge(t), owner.Counter+1,
				payload, h.now,
			)
			require.NoError(t, err)
			_, err = h.database.Queries.GetFeedBinding(t.Context(),
				sqlc.GetFeedBindingParams{
					SourceID: source.ID, SubscriptionID: id,
				})
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}
}
