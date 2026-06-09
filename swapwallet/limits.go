//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/daemonrpc"
)

// checkReceiveLimits enforces the operator-advertised limits against an
// inbound receive of amt satoshis. Two limits apply: the per-VTXO maximum
// (a Lightning receive settles into a single VTXO, so the invoice amount
// must fit in one), and the total wallet balance cap. Funds parked in
// boarding outputs count toward the balance because they are already
// committed to entering the system.
//
// Both limits are advisory zero-means-disabled values, and a daemon that
// has not yet fetched operator terms skips the checks entirely rather
// than failing closed: the operator re-validates server-side, so the
// pre-flight here exists to hand the user a clean error before an
// invoice is ever created.
func checkReceiveLimits(ctx context.Context, rpc RPCServer,
	amt btcutil.Amount) error {

	info, err := rpc.GetInfo(ctx, &daemonrpc.GetInfoRequest{})
	if err != nil {
		return fmt.Errorf("fetch operator terms: %w", err)
	}

	serverInfo := info.GetServerInfo()
	if serverInfo == nil {
		return nil
	}

	maxVTXO := btcutil.Amount(serverInfo.MaxBoardingAmount)
	if maxVTXO > 0 && amt > maxVTXO {
		return fmt.Errorf("%w: receive of %v exceeds the per-VTXO "+
			"maximum of %v", ErrAmountExceedsVTXOLimit, amt,
			maxVTXO)
	}

	maxBalance := btcutil.Amount(serverInfo.MaxUserBalance)
	if maxBalance == 0 {
		return nil
	}

	balance, err := rpc.GetBalance(ctx, &daemonrpc.GetBalanceRequest{})
	if err != nil {
		return fmt.Errorf("fetch wallet balance: %w", err)
	}

	current := btcutil.Amount(
		balance.VtxoBalanceSat +
			balance.BoardingConfirmedSat +
			balance.BoardingUnconfirmedSat +
			balance.BoardingAdoptedSat,
	)
	if current+amt > maxBalance {
		return fmt.Errorf("%w: receiving %v on top of the current "+
			"balance of %v exceeds the maximum balance of %v",
			ErrBalanceLimitExceeded, amt, current, maxBalance)
	}

	return nil
}
