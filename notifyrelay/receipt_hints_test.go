package notifyrelay

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

// subscribeHints uses the real signed management path for local presentation.
func (h *relayHarness) subscribeHints(route string) ManagementResult {
	h.t.Helper()
	response := h.manage(ManagementPayload{
		Action: "subscribe", RouteID: route, EnableAlerts: true,
		Presentation: PresentationReceiptHint,
	})
	require.Equal(h.t, http.StatusOK, response.Code)
	var result ManagementResult
	require.NoError(h.t, json.Unmarshal(response.Body.Bytes(), &result))
	require.Equal(h.t, PresentationReceiptHint, result.Presentation)

	return result
}

// TestReceiptHintCutover fences old sends and preserves event replay evidence.
func TestReceiptHintCutover(t *testing.T) {
	h := newRelayHarness(t, nil)
	sub := h.subscribe("route", true)
	require.Equal(t, PresentationGeneric, sub.Presentation)
	require.NoError(
		t,
		h.store.enqueue(
			t.Context(), sub.AlertCapability, ScopeAlert, "old",
			h.now(),
		),
	)
	old, err := h.store.claim(t.Context(), h.now())
	require.NoError(t, err)
	require.NotNil(t, old)
	hints := h.subscribeHints("route")
	require.Equal(t, sub.SubscriptionID, hints.SubscriptionID)
	require.ErrorIs(
		t,
		h.store.enqueue(
			t.Context(), sub.AlertCapability, ScopeAlert, "other",
			h.now(),
		),
		ErrNotFound,
	)
	// Same producer event remains deduplicated across capability rotation.
	require.NoError(
		t,
		h.store.enqueue(
			t.Context(), hints.AlertCapability, ScopeAlert, "old",
			h.now(),
		),
	)
	missing, err := h.store.claim(t.Context(), h.now())
	require.NoError(t, err)
	require.Nil(t, missing)
	require.NoError(
		t,
		h.store.enqueue(
			t.Context(), hints.AlertCapability, ScopeAlert, "new",
			h.now(),
		),
	)
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *old, ProviderResult{
				Outcome: OutcomeInvalidToken,
			},
			h.now(),
		),
	)
	current, err := h.store.claim(t.Context(), h.now())
	require.NoError(t, err)
	require.NotNil(t, current)
	require.Equal(t, PresentationReceiptHint, current.Presentation)
	require.Equal(t, secretHash("new"), current.EventHash)
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *current, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			h.now(),
		),
	)
	// Hints consume the payment budget, independently of ordinary wakes.
	require.NoError(
		t,
		h.store.enqueue(
			t.Context(), hints.AlertCapability, ScopeAlert, "third",
			h.now(),
		),
	)
	missing, err = h.store.claim(t.Context(), h.now())
	require.NoError(t, err)
	require.Nil(t, missing)
	h.clock.Add(int64(alertQuietPeriod + time.Second))
	current, err = h.store.claim(t.Context(), h.now())
	require.NoError(t, err)
	require.NotNil(t, current)
	// Switching back cannot replay a queued hint as a generic alert.
	h.subscribe("route", true)
	missing, err = h.store.claim(t.Context(), h.now())
	require.NoError(t, err)
	require.Nil(t, missing)
}

// TestReceiptHintAPNS proves the payment scope sends a silent fixed marker.
func TestReceiptHintAPNS(t *testing.T) {
	transport := &providerTransport{status: http.StatusOK}
	provider, _ := newTestAPNSProvider(t, transport)
	_, err := provider.Send(t.Context(), Delivery{
		DeviceToken: "aabbccdd", Topic: "com.example.wallet",
		Environment: "production", Scope: ScopeAlert,
		Presentation: PresentationReceiptHint,
		RouteID:      "route", EventHash: "opaque-hash",
	})
	require.NoError(t, err)
	require.Equal(
		t, "background", transport.request.Header.Get("Apns-Push-Type"),
	)
	var payload map[string]map[string]any
	require.NoError(t, json.Unmarshal(transport.body, &payload))
	require.Equal(t, float64(1), payload["aps"]["content-available"])
	require.NotContains(t, payload["aps"], "alert")
	require.Equal(t, "payment_activity", payload["wavelength"]["kind"])
	require.Equal(t, "opaque-hash", payload["wavelength"]["event_id"])
	wake, err := notificationPayload(Delivery{Scope: ScopeWake})
	require.NoError(t, err)
	require.NotContains(t, string(wake), "payment_activity")
}

// TestPresentationValidation limits presentation selection to alert consent.
func TestPresentationValidation(t *testing.T) {
	for _, value := range []ManagementPayload{
		{
			Action:      "rotate_token",
			DeviceToken: "aabb",
		},
		{
			Action:  "subscribe",
			RouteID: "route",
		},
		{
			Action:         "revoke_subscription",
			SubscriptionID: "sub",
		},
		{
			Action: "delete_installation",
		},
	} {
		require.True(t, validManagement(value))
		value.Presentation = PresentationReceiptHint
		require.False(t, validManagement(value))
	}
	require.False(
		t,
		validManagement(
			ManagementPayload{
				Action:       "subscribe",
				RouteID:      "route",
				EnableAlerts: true,
				Presentation: "arbitrary",
			},
		),
	)
	require.True(
		t,
		validManagement(
			ManagementPayload{
				Action:       "subscribe",
				RouteID:      "route",
				EnableAlerts: true,
				Presentation: PresentationReceiptHint,
			},
		),
	)
}

// TestRelayFormatUpgrade preserves authority and refuses unknown store formats.
func TestRelayFormatUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	store, err := OpenStore(path)
	require.NoError(t, err)
	h := newRelayHarness(t, store)
	sub := h.subscribe("route", true)
	// Format one has the same records without a presentation field.
	require.NoError(
		t, store.db.Update(func(tx *bolt.Tx) error {
			value, err := readRecord[subscription](
				tx, subscriptionsBucket, sub.SubscriptionID,
			)
			if err != nil {
				return err
			}
			value.Presentation = ""
			if err := putRecord(
				tx, subscriptionsBucket, value.ID, value,
			); err != nil {
				return err
			}

			return tx.Bucket([]byte(metadataBucket)).Put(
				[]byte("format"), []byte("1"),
			)
		}),
	)
	require.NoError(t, store.Close())
	store, err = OpenStore(path)
	require.NoError(t, err)
	require.NoError(
		t,
		store.enqueue(
			t.Context(), sub.AlertCapability, ScopeAlert, "payment",
			h.now(),
		),
	)
	claimed, err := store.claim(t.Context(), h.now())
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, PresentationGeneric, claimed.Presentation)
	require.NoError(
		t, store.db.Update(func(tx *bolt.Tx) error {
			meta := tx.Bucket([]byte(metadataBucket))
			require.Equal(
				t, "2",
				string(
					meta.Get(
						[]byte("format"),
					),
				),
			)

			return meta.Put([]byte("format"), []byte("999"))
		}),
	)
	require.NoError(t, store.Close())
	_, err = OpenStore(path)
	require.ErrorContains(
		t, err, "unsupported notification relay store format",
	)
}

// TestHintPresentationProofBinding rejects a changed mode before consuming
// proof.
func TestHintPresentationProofBinding(t *testing.T) {
	h := newRelayHarness(t, nil)
	challenge := h.challenge(OperationManage)
	action := ManagementPayload{
		Action: "subscribe", RouteID: "route", EnableAlerts: true,
		Presentation: PresentationReceiptHint,
	}
	raw, err := json.Marshal(action)
	require.NoError(t, err)
	proof := h.assertion(challenge, raw, h.counter.Add(1), h.app.AppleAppID)
	action.Presentation = PresentationGeneric
	proof.Payload, err = json.Marshal(action)
	require.NoError(t, err)
	path := "/v1/installations/" + h.ownerID + "/manage"
	require.Equal(t, http.StatusUnauthorized, h.post(path, proof, "").Code)
	proof.Payload = raw
	require.Equal(t, http.StatusOK, h.post(path, proof, "").Code)
}
