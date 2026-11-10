package notifyrelay

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestInvalidTokenRecoveryPreservesProducerAuthority distinguishes a disabled
// destination from revoked authority across the real HTTP admission path.
func TestInvalidTokenRecoveryPreservesProducerAuthority(t *testing.T) {
	h := newRelayHarness(t, nil)
	sub := h.subscribe("recoverable-route", true)
	other := h.subscribe("other-route", true)
	h.provider.send = func(context.Context, Delivery) (ProviderResult,
		error) {

		return ProviderResult{Outcome: OutcomeInvalidToken}, nil
	}
	require.Equal(
		t, http.StatusAccepted, h.post(
			"/v1/wakes",
			PushRequest{EventID: "first"},
			sub.WakeCapability,
		).Code,
	)
	worked, err := h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.True(t, worked)

	// A producer must retain this grant and retry after token recovery. A
	// 401 would incorrectly tell it to permanently retire its source
	// binding.
	for _, request := range []struct{ path, capability string }{
		{
			"/v1/wakes",
			sub.WakeCapability,
		},
		{
			"/v1/alerts",
			sub.AlertCapability,
		},
	} {
		require.Equal(
			t, http.StatusServiceUnavailable, h.post(
				request.path,
				PushRequest{EventID: "retry"},
				request.capability,
			).Code,
		)
	}
	verification := SubscriptionVerificationRequest{
		SubscriptionID:  sub.SubscriptionID,
		AlertCapability: sub.AlertCapability,
	}
	require.Equal(
		t, http.StatusServiceUnavailable, h.post(
			"/v1/subscriptions/verify",
			verification,
			sub.WakeCapability,
		).Code,
	)
	require.Equal(
		t, http.StatusUnauthorized, h.post(
			"/v1/wakes",
			PushRequest{EventID: "unknown"},
			"unknown-capability",
		).Code,
	)
	// Destination failure cannot turn a mismatched binding into a retry.
	for _, mixed := range []SubscriptionVerificationRequest{
		{
			SubscriptionID: other.SubscriptionID,
		},
		{
			SubscriptionID:  sub.SubscriptionID,
			AlertCapability: other.AlertCapability,
		},
	} {
		require.Equal(
			t, http.StatusUnauthorized, h.post(
				"/v1/subscriptions/verify",
				mixed, sub.WakeCapability,
			).Code,
		)
	}
	worked, err = h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.False(
		t, worked, "disabled tokens must not receive another send",
	)

	require.Equal(
		t, http.StatusOK, h.manage(ManagementPayload{
			Action:      "rotate_token",
			DeviceToken: "eeff0011",
		}).Code,
	)
	require.Equal(
		t, http.StatusOK, h.post(
			"/v1/subscriptions/verify",
			verification,
			sub.WakeCapability,
		).Code,
	)
	h.provider.send = func(_ context.Context, delivery Delivery) (
		ProviderResult, error) {

		require.Equal(t, "eeff0011", delivery.DeviceToken)

		return ProviderResult{Outcome: OutcomeAccepted}, nil
	}
	require.Equal(
		t, http.StatusAccepted, h.post(
			"/v1/wakes",
			PushRequest{EventID: "retry"},
			sub.WakeCapability,
		).Code,
	)
	worked, err = h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
	require.Equal(
		t, http.StatusOK, h.manage(ManagementPayload{
			Action:         "revoke_subscription",
			SubscriptionID: sub.SubscriptionID,
		}).Code,
	)
	require.Equal(
		t, http.StatusUnauthorized, h.post(
			"/v1/wakes",
			PushRequest{EventID: "revoked"},
			sub.WakeCapability,
		).Code,
	)
}

// TestDisabledDestinationStillRejectsDeletedAuthority ensures temporary token
// unavailability cannot hide permanent installation deletion.
func TestDisabledDestinationStillRejectsDeletedAuthority(t *testing.T) {
	h := newRelayHarness(t, nil)
	sub := h.subscribe("deleted-route", false)
	h.provider.send = func(context.Context, Delivery) (ProviderResult,
		error) {

		return ProviderResult{Outcome: OutcomeInvalidToken}, nil
	}
	require.Equal(
		t, http.StatusAccepted, h.post(
			"/v1/wakes",
			PushRequest{EventID: "first"},
			sub.WakeCapability,
		).Code,
	)
	worked, err := h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
	require.Equal(
		t, http.StatusOK,
		h.manage(ManagementPayload{Action: "delete_installation"}).Code,
	)
	require.Equal(
		t, http.StatusUnauthorized, h.post(
			"/v1/wakes",
			PushRequest{EventID: "deleted"},
			sub.WakeCapability,
		).Code,
	)
	require.Equal(
		t, http.StatusUnauthorized, h.post(
			"/v1/subscriptions/verify",
			SubscriptionVerificationRequest{
				SubscriptionID: sub.SubscriptionID,
			}, sub.WakeCapability,
		).Code,
	)
}
