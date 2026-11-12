package notifyrelay

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

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
