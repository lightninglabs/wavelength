//go:build swapruntime

package swapclientserver

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/waved"
)

// errInvalidNotificationIssuer rejects malformed pins before setup side
// effects.
var errInvalidNotificationIssuer = errors.New("invalid swap notification " +
	"issuer pin")

// parseSwapNotificationIssuer validates an optional pin before the subserver
// opens storage, installs signer hooks or chooses its transport. REST does not
// support registration, but malformed configuration still fails consistently.
func parseSwapNotificationIssuer(cfg *waved.SwapConfig) (*btcec.PublicKey,
	error) {

	if cfg == nil || cfg.NotificationIssuer == "" {
		return nil, nil
	}
	raw, err := hex.DecodeString(cfg.NotificationIssuer)
	if err != nil || len(raw) != 33 {
		return nil, errInvalidNotificationIssuer
	}
	issuer, err := btcec.ParsePubKey(raw)
	if err != nil {
		return nil, errInvalidNotificationIssuer
	}

	return issuer, nil
}

// swapNotificationRegistrar binds signing to the configured gRPC transport and
// a previously validated issuer pin. An unpinned challenge cannot choose the
// audience the wallet signs for, including this wallet's operator.
func (s *swapClientService) swapNotificationRegistrar(issuer *btcec.PublicKey,
	signer *waved.RPCServer) waved.NotificationRegistrar {

	if issuer == nil || s.notificationRPC == nil {
		return nil
	}

	return func(ctx context.Context,
		mutation *pb.NotificationSubscriptionMutation) (
		*pb.NotificationSubscriptionResponse, error) {

		return signer.AuthorizeNotificationSubscription(
			ctx, s.notificationRPC, issuer, mutation,
		)
	}
}
