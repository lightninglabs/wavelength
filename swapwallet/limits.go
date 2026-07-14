//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/waverpc"
)

// checkReceiveLimits enforces the operator-advertised limits against an
// inbound receive of amt satoshis. Two limits apply: the per-VTXO maximum
// (a Lightning receive settles into a single VTXO, so the invoice amount
// must fit in one), and the total wallet balance cap. Funds parked in
// boarding outputs count toward the balance because they are already
// committed to entering the system.
//
// NOTE: this "current balance" sums live VTXOs plus EVERY boarding bucket
// (confirmed, unconfirmed, adopted) because a receive adds funds on top of
// everything the wallet already holds. The boarding path's
// wallet.boardingHeadroom deliberately uses a NARROWER definition (it
// excludes the confirmed boarding balance it is converting). The two are
// intentionally different per flow; see boardingHeadroom for the rationale.
// Neither counts value promised by an in-flight round that has not yet
// confirmed (projected separately as VTXO_STATUS_PENDING_ROUND); the
// operator re-validates at round time, so this advisory pre-flight can
// briefly under-count without consequence.
//
// Both limits are advisory zero-means-disabled training-wheels values, and
// the check fails OPEN throughout: a daemon that has not yet fetched terms,
// or that hits a transient GetInfo/GetBalance error, skips the affected
// check rather than blocking a legitimate receive. The operator re-validates
// VTXO creation server-side, so this pre-flight exists only to hand the user
// a clean error before an invoice is created -- never as a security boundary.
func checkReceiveLimits(ctx context.Context, rpc RPCServer, log btclog.Logger,
	amt btcutil.Amount) error {

	info, err := rpc.GetInfo(ctx, &waverpc.GetInfoRequest{})
	if err != nil {
		log.WarnS(ctx, "Skipping receive limit pre-flight: operator "+
			"terms unavailable", err)

		return nil
	}

	serverInfo := info.GetServerInfo()
	if serverInfo == nil {
		return nil
	}

	maxVTXO := btcutil.Amount(serverInfo.MaxVtxoAmount)
	if maxVTXO > 0 && amt > maxVTXO {
		return fmt.Errorf("%w: receive of %v exceeds the per-VTXO "+
			"maximum of %v", ErrAmountExceedsVTXOLimit, amt,
			maxVTXO)
	}

	maxBalance := btcutil.Amount(serverInfo.MaxUserBalance)
	if maxBalance == 0 {
		return nil
	}

	balance, err := rpc.GetBalance(ctx, &waverpc.GetBalanceRequest{})
	if err != nil {
		log.WarnS(ctx, "Skipping receive balance-cap pre-flight: "+
			"wallet balance unavailable", err)

		return nil
	}

	// Fail open on a nil balance, mirroring the serverInfo guard above:
	// a real backend never returns (nil, nil), but a mock or test seam
	// might, and the advisory cap must never panic the receive path.
	if balance == nil {
		return nil
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
