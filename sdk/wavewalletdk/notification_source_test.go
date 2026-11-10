package wavewalletdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notification"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// sourceTestEdge accepts real full-request proofs under one distinct source's
// challenge key. A route sent to the wrong source cannot verify its MAC.
type sourceTestEdge struct {
	authority *notification.ChallengeAuthority
	calls     int
}

// GetNotificationChallenge returns authority owned by this configured source.
func (s *sourceTestEdge) GetNotificationChallenge(_ context.Context,
	request *pb.NotificationChallengeRequest, _ ...grpc.CallOption) (
	*pb.NotificationChallenge, error) {

	return s.authority.NewChallenge(request.OwnerPubkey, time.Now())
}

// SetNotificationSubscription verifies both issuer MAC and exact owner proof.
func (s *sourceTestEdge) SetNotificationSubscription(_ context.Context,
	request *pb.NotificationSubscriptionRequest, _ ...grpc.CallOption) (
	*pb.NotificationSubscriptionResponse, error) {

	if err := s.authority.VerifyChallenge(
		request.Challenge, time.Now(),
	); err != nil {
		return nil, err
	}
	if err := notification.VerifyAuthorization(
		request, time.Now(),
	); err != nil {
		return nil, err
	}
	s.calls++

	return &pb.NotificationSubscriptionResponse{
		SubscriptionId: request.Mutation.SubscriptionId,
		ExpiresAtUnix: request.
			Mutation.
			ExpiresAtUnix,
	}, nil
}

// TestNotificationSourcesStaySeparate drives the facade through real signed
// challenge exchanges with distinct issuers. A source cannot choose a URL or
// borrow the other's identity; unknown selectors fail before any wallet
// signing.
func TestNotificationSourcesStaySeparate(t *testing.T) {
	owner, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	swap, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorAuthority, err := notification.NewChallengeAuthority(
		bytes.Repeat(
			[]byte{1}, 32,
		),
		operator.PubKey(),
	)
	require.NoError(t, err)
	swapAuthority, err := notification.NewChallengeAuthority(
		bytes.Repeat(
			[]byte{2}, 32,
		),
		swap.PubKey(),
	)
	require.NoError(t, err)
	operatorEdge := &sourceTestEdge{authority: operatorAuthority}
	swapEdge := &sourceTestEdge{authority: swapAuthority}
	signs := 0
	sign := func(_ context.Context, message, tag []byte) (
		*schnorr.Signature, error) {

		signs++
		digest := chainhash.TaggedHash(tag, message)

		return schnorr.Sign(owner, digest[:])
	}
	client := &Client{
		setNotificationSubscription: func(ctx context.Context,
			mutation *pb.NotificationSubscriptionMutation) (
			*pb.NotificationSubscriptionResponse, error) {

			return notification.SetSubscription(
				ctx, operatorEdge, owner.PubKey(),
				operator.PubKey(), mutation, sign,
			)
		},
		setSwapNotificationSubscription: func(ctx context.Context,
			mutation *pb.NotificationSubscriptionMutation) (
			*pb.NotificationSubscriptionResponse, error) {

			return notification.SetSubscription(
				ctx, swapEdge, owner.PubKey(), swap.PubKey(),
				mutation, sign,
			)
		},
	}
	request := NotificationSubscription{
		SubscriptionID: "route",
		RelayID:        "app",
		WakeCapability: base64.RawURLEncoding.EncodeToString(
			bytes.Repeat(
				[]byte{3}, 32,
			),
		),
		ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
	}
	result, err := client.SetNotificationSubscription(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, request.SubscriptionID, result.SubscriptionID)
	require.Equal(t, 1, operatorEdge.calls)
	require.Zero(t, swapEdge.calls)
	request.Source = NotificationSourceSwapServer
	_, err = client.SetNotificationSubscription(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, 1, operatorEdge.calls)
	require.Equal(t, 1, swapEdge.calls)
	request.Source = "https://untrusted.invalid"
	_, err = client.SetNotificationSubscription(t.Context(), request)
	require.ErrorIs(t, err, ErrInvalidNotificationSource)
	require.Equal(t, 2, signs)
	request.Source = NotificationSourceSwapServer
	_, err = (&Client{}).SetNotificationSubscription(t.Context(), request)
	require.ErrorIs(t, err, notification.ErrUnsupported)
}
