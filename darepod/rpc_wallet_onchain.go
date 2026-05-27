package darepod

import "context"

// NewWalletAddress returns a fresh receive address from the backing Bitcoin
// wallet through the RPCServer facade.
func (r *RPCServer) NewWalletAddress(ctx context.Context) (string, error) {
	return r.server.NewWalletAddress(ctx)
}

// SendWalletOnchain pays an address from the backing Bitcoin wallet through
// the RPCServer facade.
func (r *RPCServer) SendWalletOnchain(ctx context.Context, address string,
	amtSat uint64, note string) (string, error) {

	return r.server.SendWalletOnchain(ctx, address, amtSat, note)
}
