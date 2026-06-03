package darepod

import (
	"context"

	"github.com/lightninglabs/darepo-client/wallet"
)

// NewWalletAddress returns a fresh backing-wallet receive address for wallet
// RPC facade code that needs an internal cooperative-exit destination.
func (r *RPCServer) NewWalletAddress(ctx context.Context) (string, error) {
	return r.server.NewWalletAddress(ctx)
}

// ListWalletUnspent returns confirmed backing-wallet UTXOs for wallet RPC
// facade preflight checks.
func (r *RPCServer) ListWalletUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*wallet.Utxo, error) {

	return r.server.ListWalletUnspent(ctx, minConfs, maxConfs)
}
