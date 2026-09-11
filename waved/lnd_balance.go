package waved

import (
	"context"

	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// lndWalletBalance reports only the account this daemon spends from. Both the
// balance RPC and metrics use this boundary so co-tenant funds stay excluded.
// The raw client preserves lndclient's authentication and configured timeout;
// lndclient.WalletBalance has no account selector and sums the whole node.
func (s *Server) lndWalletBalance(ctx context.Context,
	client lndclient.LightningClient) (*lnrpc.WalletBalanceResponse,
	error) {

	ctx, timeout, raw := client.RawClientWithMacAuth(ctx)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return raw.WalletBalance(ctx, &lnrpc.WalletBalanceRequest{
		Account: s.lndWalletAccount(),
	})
}
