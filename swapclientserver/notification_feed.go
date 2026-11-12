//go:build swapruntime

package swapclientserver

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/waved"
)

// swapFeedRegistrar binds signing to the configured gRPC transport and
// a previously validated issuer pin. An unpinned challenge cannot choose the
// audience the wallet signs for, including this wallet's operator.
func (s *swapClientService) swapFeedRegistrar(issuer *btcec.PublicKey,
	signer *waved.RPCServer) waved.FeedGrantRegistrar {

	if issuer == nil || s.feedRPC == nil {
		return nil
	}

	return func(ctx context.Context, mutation *pb.FeedGrantMutation) (
		*pb.FeedGrantResponse, error) {

		return signer.AuthorizeFeedGrant(
			ctx, s.feedRPC, issuer, mutation,
		)
	}
}
