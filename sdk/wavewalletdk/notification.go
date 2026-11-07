package wavewalletdk

import (
	"context"

	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notification"
)

// NotificationSubscription is one app-owned relay route for this wallet.
// Capabilities are private credentials. RelayID selects an operator allowlist
// entry, never a URL. ExpiresAtUnix must not exceed the relay capability
// expiry. Remove needs only SubscriptionID and forbids all other routing
// fields.
type NotificationSubscription struct {
	SubscriptionID  string `json:"subscription_id"`
	RelayID         string `json:"relay_id,omitempty"`
	WakeCapability  string `json:"wake_capability,omitempty"`
	AlertCapability string `json:"alert_capability,omitempty"`
	ExpiresAtUnix   int64  `json:"expires_at_unix,omitempty"`
	Remove          bool   `json:"remove,omitempty"`
}

// NotificationSubscriptionResult confirms the saved ID and expiry without
// returning capabilities. A removal has zero expiry.
type NotificationSubscriptionResult struct {
	SubscriptionID string `json:"subscription_id"`
	ExpiresAtUnix  int64  `json:"expires_at_unix"`
}

// SetNotificationSubscription binds an embedded wallet's mailbox to an
// app-issued relay capability through fresh full-request authorization. It
// returns notification.ErrUnsupported for remote handles or older operators;
// callers retain foreground recovery. It does not enable wallet background
// execution or create an APNs enrollment on the app's behalf.
func (c *Client) SetNotificationSubscription(ctx context.Context,
	request NotificationSubscription) (*NotificationSubscriptionResult,
	error) {

	if c == nil || c.setNotificationSubscription == nil {
		return nil, notification.ErrUnsupported
	}
	action := pb.NotificationSubscriptionMutation_ACTION_UPSERT
	if request.Remove {
		action = pb.NotificationSubscriptionMutation_ACTION_REMOVE
	}
	result, err := c.setNotificationSubscription(ctx,
		&pb.NotificationSubscriptionMutation{
			Action: action, SubscriptionId: request.SubscriptionID,
			RelayId:         request.RelayID,
			WakeCapability:  request.WakeCapability,
			AlertCapability: request.AlertCapability,
			ExpiresAtUnix:   request.ExpiresAtUnix,
		})
	if err != nil {
		return nil, err
	}

	return &NotificationSubscriptionResult{
		SubscriptionID: result.GetSubscriptionId(),
		ExpiresAtUnix:  result.GetExpiresAtUnix(),
	}, nil
}
