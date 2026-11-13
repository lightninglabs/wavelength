package swaps

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/lib/swaprecovery"
	"github.com/lightningnetwork/lnd/lntypes"
)

// newOutSwapPreimage generates public recovery metadata before invoice
// registration. Nonce uniqueness allows index zero without a restore cursor.
func newOutSwapPreimage(ctx context.Context, daemon DaemonConn) (
	lntypes.Preimage, []byte, error) {

	params, err := swaprecovery.NewParams(0)
	if err != nil {
		return lntypes.Preimage{}, nil, err
	}
	preimage, err := daemon.DeriveSwapPreimage(ctx, params)
	if err != nil {
		return lntypes.Preimage{}, nil, err
	}

	return preimage, params, nil
}

// recoverOutSwapPreimage verifies public metadata against the payment hash
// before a restored client supplies the result to a claim executor.
func recoverOutSwapPreimage(ctx context.Context, daemon DaemonConn,
	paymentHash lntypes.Hash, params []byte) (*lntypes.Preimage, error) {

	if _, err := swaprecovery.ParseParams(params); err != nil {
		return nil, malformedRecoverableSwap(err)
	}
	preimage, err := daemon.DeriveSwapPreimage(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("derive recovered preimage: %w", err)
	}
	if preimage.Hash() != paymentHash {
		return nil, malformedRecoverableSwap(
			fmt.Errorf("recovery preimage hash mismatch"),
		)
	}

	return &preimage, nil
}
