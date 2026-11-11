package wavewalletdk

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notification"
)

// NotificationFeedGrant delegates wake hints to a publisher's mailbox key.
// Source selects a configured producer; it cannot be an endpoint from a push.
// SubscriberPubkey is compressed public-key hex, not a wallet signing key.
// Removal uses only Source, GrantID and Remove.
type NotificationFeedGrant struct {
	Source NotificationSource `json:"source,omitempty"`

	GrantID             string `json:"grant_id"`
	SubscriberPubkey    string `json:"subscriber_pubkey,omitempty"`
	SubscriberSignature string `json:"subscriber_signature,omitempty"`
	EventClasses        uint32 `json:"event_classes,omitempty"`
	ExpiresAtUnix       int64  `json:"expires_at_unix,omitempty"`
	Remove              bool   `json:"remove,omitempty"`
}

// NotificationFeedGrantResult returns producer-scoped correlation, never read
// authority. FeedToken uses canonical raw URL-base64; removal returns empty.
type NotificationFeedGrantResult struct {
	GrantID       string `json:"grant_id"`
	FeedToken     string `json:"feed_token"`
	ExpiresAtUnix int64  `json:"expires_at_unix"`
}

// SetNotificationFeedGrant uses the embedded wallet's identity to authorize a
// subscriber at one configured producer. It creates no device enrollment or
// background execution. Remote handles and unavailable sources return
// notification.ErrUnsupported. A timeout can follow commit; retry the same
// grant ID with a fresh invocation.
func (c *Client) SetNotificationFeedGrant(ctx context.Context,
	request NotificationFeedGrant) (*NotificationFeedGrantResult, error) {

	if c == nil {
		return nil, notification.ErrUnsupported
	}
	register := c.setFeedGrant
	switch request.Source {
	case "",
		NotificationSourceOperator:
	case NotificationSourceSwapServer:
		register = c.setSwapFeedGrant

	default:
		return nil, ErrInvalidNotificationSource
	}
	if register == nil {
		return nil, notification.ErrUnsupported
	}
	subscriber, err := hex.DecodeString(request.SubscriberPubkey)
	if err != nil {
		return nil, fmt.Errorf("subscriber_pubkey: %w",
			notification.ErrInvalidAuthorization)
	}
	consent, err := base64.RawURLEncoding.DecodeString(
		request.SubscriberSignature,
	)
	if err != nil {
		return nil, fmt.Errorf("subscriber_signature: %w",
			notification.ErrInvalidAuthorization)
	}
	action := pb.FeedGrantMutation_ACTION_UPSERT
	if request.Remove {
		action = pb.FeedGrantMutation_ACTION_REMOVE
	}
	callCtx, release, err := c.beginLocalCall(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	response, err := register(callCtx, &pb.FeedGrantMutation{
		Action:           action,
		GrantId:          request.GrantID,
		SubscriberPubkey: subscriber, SubscriberSignature: consent,
		EventClasses:  request.EventClasses,
		ExpiresAtUnix: request.ExpiresAtUnix,
	})
	if err != nil {
		return nil, err
	}

	return &NotificationFeedGrantResult{
		GrantID: response.GetGrantId(),
		FeedToken: base64.RawURLEncoding.EncodeToString(
			response.GetFeedToken(),
		),
		ExpiresAtUnix: response.GetExpiresAtUnix(),
	}, nil
}
