package waved

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notification"
	"github.com/lightninglabs/wavelength/waverpc"
)

// SetNotificationFeedGrant authorizes one subscriber delegation under this
// wallet's mailbox key. It uses the bootstrapped operator connection and wallet
// signer; neither a notification nor a host supplies a signing key or URL.
// Wallet opening and operator negotiation must complete first. It creates no
// implicit long-lived work: both RPCs and signing belong to ctx.
func (s *Server) SetNotificationFeedGrant(ctx context.Context,
	mutation *mailboxpb.FeedGrantMutation) (*mailboxpb.FeedGrantResponse,
	error) {

	conn, owner, source, err := s.notificationSource()
	if err != nil {
		return nil, err
	}

	return notification.SetFeedGrant(
		ctx, mailboxpb.NewWakeFeedServiceClient(conn), owner, source,
		mutation, s.signNotificationGrant,
	)

}

// FeedGrantRegistrar sends a signed mutation to one configured source.
// It is an in-process lifecycle hook, never a remote wallet-signing RPC.
type FeedGrantRegistrar func(context.Context,
	*mailboxpb.FeedGrantMutation) (
	*mailboxpb.FeedGrantResponse, error)

// RegisterSwapFeedSource publishes the configured swap registrar before
// the subserver accepts calls. The mutex also covers registration racing wallet
// startup; no caller reads a mutable Config.Backend pointer to find its signer.
func (r *RPCServer) RegisterSwapFeedSource(registrar FeedGrantRegistrar) {
	r.server.swapNotificationMu.Lock()
	defer r.server.swapNotificationMu.Unlock()
	r.server.swapFeedRegistrar = registrar
}

// SetSwapFeedGrant registers only with the configured swap
// source. Builds or transports without that source retain foreground recovery.
func (s *Server) SetSwapFeedGrant(ctx context.Context,
	mutation *mailboxpb.FeedGrantMutation) (*mailboxpb.FeedGrantResponse,
	error) {

	s.swapNotificationMu.RLock()
	registrar := s.swapFeedRegistrar
	s.swapNotificationMu.RUnlock()
	if registrar == nil {
		return nil, notification.ErrUnsupported
	}

	return registrar(ctx, mutation)
}

// AuthorizeFeedGrant is a programmatic signer bridge for a
// configured source with an explicitly pinned issuer. It uses this wallet's
// identity without waiting for ingress and never exposes raw key material.
// Callers must own the configured transport; push payloads cannot select it.
func (r *RPCServer) AuthorizeFeedGrant(ctx context.Context,
	remote mailboxpb.WakeFeedServiceClient, issuer *btcec.PublicKey,
	mutation *mailboxpb.FeedGrantMutation) (*mailboxpb.FeedGrantResponse,
	error) {

	identity := r.server.loadClientKeyDesc()
	if identity.PubKey == nil {
		return nil, waverpc.WalletNotReadyError(
			"notification identity unavailable",
		)
	}

	return notification.SetFeedGrant(
		ctx, remote, identity.PubKey, issuer, mutation,
		r.server.signNotificationGrant,
	)
}
