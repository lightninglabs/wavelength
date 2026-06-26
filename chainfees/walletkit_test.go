package chainfees

import (
	"context"
	"fmt"
	"testing"

	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

type testWalletKit struct {
	lndclient.WalletKitClient

	rate      chainfee.SatPerKWeight
	err       error
	gotTarget int32
}

func (w *testWalletKit) EstimateFeeRate(_ context.Context, target int32) (
	chainfee.SatPerKWeight, error) {

	w.gotTarget = target

	if w.err != nil {
		return 0, w.err
	}

	return w.rate, nil
}

func TestWalletKitEstimatorReturnsSatPerKW(t *testing.T) {
	t.Parallel()

	const wantRate = chainfee.SatPerKWeight(1_250)

	walletKit := &testWalletKit{
		rate: wantRate,
	}
	estimator := NewWalletKitEstimator(walletKit, nil)

	gotRate, err := estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, wantRate, gotRate)
	require.Equal(t, int32(6), walletKit.gotTarget)
}

func TestWalletKitEstimatorClampsSubFloorRate(t *testing.T) {
	t.Parallel()

	walletKit := &testWalletKit{
		rate: 1,
	}
	estimator := NewWalletKitEstimator(walletKit, nil)

	gotRate, err := estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, chainfee.FeePerKwFloor, gotRate)
}

func TestWalletKitEstimatorFallsBackToLastGoodRate(t *testing.T) {
	t.Parallel()

	walletKit := &testWalletKit{
		rate: 1_200,
	}
	estimator := NewFallbackWalletKitEstimator(walletKit, nil)

	gotRate, err := estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, walletKit.rate, gotRate)

	walletKit.err = fmt.Errorf("offline")
	gotRate, err = estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, walletKit.rate, gotRate)
}

func TestWalletKitEstimatorFallsBackToFloorBeforeFirstSuccess(t *testing.T) {
	t.Parallel()

	walletKit := &testWalletKit{
		err: fmt.Errorf("offline"),
	}
	estimator := NewFallbackWalletKitEstimator(walletKit, nil)

	// With no successful estimate cached yet, a fallback estimator returns
	// the relay floor (not an error).
	gotRate, err := estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, chainfee.FeePerKwFloor, gotRate)
}

func TestWalletKitEstimatorFailsFastByDefault(t *testing.T) {
	t.Parallel()

	walletKit := &testWalletKit{
		err: fmt.Errorf("offline"),
	}
	estimator := NewWalletKitEstimator(walletKit, nil)

	_, err := estimator.EstimateFeePerKW(6)
	require.ErrorContains(t, err, "estimate walletkit fee rate")
}
