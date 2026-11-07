package waved

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
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

// NotificationRegistrar sends a signed mutation to one configured source.
// It is an in-process lifecycle hook, never a remote wallet-signing RPC.
type NotificationRegistrar func(context.Context,
	*mailboxpb.NotificationSubscriptionMutation) (
	*mailboxpb.NotificationSubscriptionResponse, error)

// RegisterSwapNotificationSource publishes the configured swap registrar before
// the subserver accepts calls. The mutex also covers registration racing wallet
// startup; no caller reads a mutable Config.Backend pointer to find its signer.
func (r *RPCServer) RegisterSwapNotificationSource(
	registrar NotificationRegistrar) {

	r.server.swapNotificationMu.Lock()
	defer r.server.swapNotificationMu.Unlock()
	r.server.swapNotificationRegistrar = registrar
}

// SetSwapNotificationSubscription registers only with the configured swap
// source. Builds or transports without that source retain foreground recovery.
func (s *Server) SetSwapNotificationSubscription(ctx context.Context,
	mutation *mailboxpb.NotificationSubscriptionMutation) (
	*mailboxpb.NotificationSubscriptionResponse, error) {

	s.swapNotificationMu.RLock()
	registrar := s.swapNotificationRegistrar
	s.swapNotificationMu.RUnlock()
	if registrar == nil {
		return nil, notification.ErrUnsupported
	}

	return registrar(ctx, mutation)
}

// AuthorizeNotificationSubscription is a programmatic signer bridge for a
// configured source with an explicitly pinned issuer. It uses this wallet's
// identity without waiting for ingress and never exposes raw key material.
// Callers must own the configured transport; push payloads cannot select it.
func (r *RPCServer) AuthorizeNotificationSubscription(ctx context.Context,
	remote mailboxpb.NotificationServiceClient, issuer *btcec.PublicKey,
	mutation *mailboxpb.NotificationSubscriptionMutation) (
	*mailboxpb.NotificationSubscriptionResponse, error) {

	identity := r.server.loadClientKeyDesc()
	if identity.PubKey == nil {
		return nil, waverpc.WalletNotReadyError(
			"notification identity unavailable",
		)
	}
	sign := func(ctx context.Context, message, tag []byte) (
		*schnorr.Signature, error) {

		return r.server.signTaggedSchnorr(
			ctx, message, tag, "notification subscription",
		)
	}

	return notification.SetSubscription(
		ctx, remote, identity.PubKey, issuer, mutation, sign,
	)
}
