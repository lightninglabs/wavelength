package waved

import (
	"github.com/lightninglabs/lndclient"
)

// lndServices is the seam over a connected lnd backend. Both the native gRPC
// lndclient (via grpcLndServices) and the REST-backed lndrest backend satisfy
// it, so the daemon can talk to lnd over either transport without the rest of
// the server branching on the concrete connection type. It exposes the shared
// *lndclient.LndServices payload (Signer / WalletKit / ChainKit /
// ChainNotifier / Client plus ChainParams / NodeAlias / NodePubkey) and the
// resource-releasing Close.
type lndServices interface {
	// Services returns the populated lndclient service payload. Callers
	// read the interface-typed subsystem clients (Signer, WalletKit,
	// ChainKit, ChainNotifier, Client) and the connection metadata
	// (ChainParams, NodeAlias, NodePubkey) from it.
	Services() *lndclient.LndServices

	// Close releases the resources held by the backend (the gRPC
	// connection for the native path, streaming goroutines for REST).
	Close()
}

// grpcLndServices adapts the native gRPC *lndclient.GrpcLndServices to the
// lndServices seam. GrpcLndServices embeds LndServices by value and already
// has Close, so the only method it lacks is the Services accessor, which
// returns the address of the embedded payload.
type grpcLndServices struct {
	*lndclient.GrpcLndServices
}

// Services returns the embedded lndclient service payload.
func (g *grpcLndServices) Services() *lndclient.LndServices {
	return &g.GrpcLndServices.LndServices
}

// A compile-time check that grpcLndServices satisfies the lndServices seam.
var _ lndServices = (*grpcLndServices)(nil)
