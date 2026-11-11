package wavewalletdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notification"
	"github.com/stretchr/testify/require"
)

// TestNotificationFeedFacade verifies the public JSON-shaped boundary and
// configured-source selection, independently of the native signing tests.
func TestNotificationFeedFacade(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	publicKey := key.PubKey().SerializeCompressed()
	consent := bytes.Repeat([]byte{4}, 64)
	token := bytes.Repeat([]byte{0xff}, 32)
	request := NotificationFeedGrant{
		GrantID:          "publisher-route",
		SubscriberPubkey: hex.EncodeToString(publicKey),
		SubscriberSignature: base64.RawURLEncoding.EncodeToString(
			consent,
		),
		EventClasses:  3,
		ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
	}
	tests := []struct {
		name     string
		change   func(*NotificationFeedGrant)
		wantErr  error
		wantSwap bool
	}{
		{
			name: "operator",
		},
		{name: "swap", wantSwap: true, change: func(
			r *NotificationFeedGrant) {

			r.Source = NotificationSourceSwapServer
		}},
		{name: "remove", change: func(r *NotificationFeedGrant) {
			*r = NotificationFeedGrant{
				GrantID: r.GrantID,
				Remove:  true,
			}
		}},
		{name: "bad key",
			wantErr: notification.ErrInvalidAuthorization,
			change: func(r *NotificationFeedGrant) {
				r.SubscriberPubkey = "zz"
			}},
		{name: "bad consent",
			wantErr: notification.ErrInvalidAuthorization,
			change: func(r *NotificationFeedGrant) {
				r.SubscriberSignature = "!"
			}},
		{name: "unconfigured source",
			wantErr: ErrInvalidNotificationSource,
			change: func(r *NotificationFeedGrant) {
				r.Source = "https://invalid"
			}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := request
			if test.change != nil {
				test.change(&r)
			}
			calls := 0
			var captured *pb.FeedGrantMutation
			usedSwap := false
			callback := func(_ context.Context,
				m *pb.FeedGrantMutation) (*pb.FeedGrantResponse,
				error) {

				calls++
				captured = m
				response := &pb.FeedGrantResponse{
					GrantId: m.GrantId,
				}
				if !r.Remove {
					response.FeedToken = token
					response.ExpiresAtUnix = m.ExpiresAtUnix
				}

				return response, nil
			}
			client := &Client{
				setFeedGrant: callback,
				setSwapFeedGrant: func(ctx context.Context,
					m *pb.FeedGrantMutation) (
					*pb.FeedGrantResponse, error) {

					usedSwap = true

					return callback(ctx, m)
				},
			}
			result, err := client.SetNotificationFeedGrant(
				t.Context(), r,
			)
			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
				require.Zero(t, calls)

				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			require.Equal(t, test.wantSwap, usedSwap)
			require.Equal(t, r.GrantID, captured.GrantId)
			if r.Remove {
				require.Equal(
					t, pb.FeedGrantMutation_ACTION_REMOVE,
					captured.Action,
				)
				require.Empty(t, captured.SubscriberPubkey)
				require.Empty(t, captured.SubscriberSignature)
				require.Zero(t, captured.EventClasses)
				require.Zero(t, captured.ExpiresAtUnix)
			} else {
				require.Equal(
					t, pb.FeedGrantMutation_ACTION_UPSERT,
					captured.Action,
				)
				require.Equal(
					t, publicKey, captured.SubscriberPubkey,
				)
				require.Equal(
					t, consent,
					captured.SubscriberSignature,
				)
				require.Equal(
					t, r.EventClasses,
					captured.EventClasses,
				)
				require.Equal(
					t, r.ExpiresAtUnix,
					captured.ExpiresAtUnix,
				)
			}

			require.Equal(t, r.GrantID, result.GrantID)
			if r.Remove {
				require.Empty(t, result.FeedToken)
			} else {
				require.Equal(
					t,
					base64.RawURLEncoding.EncodeToString(
						token,
					),
					result.FeedToken,
				)
			}
		})
	}
}
