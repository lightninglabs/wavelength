package waved

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notification"
	"github.com/lightninglabs/wavelength/waverpc"
)

// SetNotificationSubscription authorizes one fixed-relay route under this
// wallet's mailbox key. It uses the bootstrapped operator connection and wallet
// signer; neither a notification nor a host supplies a signing key or URL.
// Wallet opening and operator negotiation must complete first. It creates no
// implicit long-lived work: both RPCs and signing belong to ctx.
func (s *Server) SetNotificationSubscription(ctx context.Context,
	mutation *mailboxpb.NotificationSubscriptionMutation) (
	*mailboxpb.NotificationSubscriptionResponse, error) {

	s.operatorConnMu.RLock()
	conn := s.serverConn
	terms := s.loadOperatorTerms()
	identity := s.loadClientKeyDesc()
	s.operatorConnMu.RUnlock()
	if conn == nil || terms == nil || terms.PubKey == nil ||
		identity.PubKey == nil {
		return nil, waverpc.WalletNotReadyError(
			"notification signer or operator connection " +
				"unavailable",
		)
	}
	sign := func(ctx context.Context, message, tag []byte) (
		*schnorr.Signature, error) {

		return s.signTaggedSchnorr(
			ctx, message, tag, "notification subscription",
		)
	}

	return notification.SetSubscription(
		ctx, mailboxpb.NewNotificationServiceClient(conn),
		identity.PubKey, terms.PubKey, mutation, sign,
	)
}
