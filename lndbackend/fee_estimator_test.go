package lndbackend

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// estimateFeeRateOverride lets mockWalletKit swap in a custom
// EstimateFeeRate implementation per test without reimplementing
// the full lndclient.WalletKitClient surface.
type estimateFeeRateOverride func(ctx context.Context,
	confTarget int32) (chainfee.SatPerKWeight, error)

// mockWalletKit is a minimal lndclient.WalletKitClient stub for
// exercising the estimator's EstimateFeeRate call + error fallback.
// All other methods of the interface panic on call so a future test
// that accidentally reaches one surfaces loudly rather than silently
// using a zero value.
type mockWalletKit struct {
	lndclient.WalletKitClient

	overrideEstimateFeeRate estimateFeeRateOverride

	// gotTarget records the last confTarget WalletKit was called
	// with so tests can assert the estimator passes it through.
	gotTarget int32
}

// EstimateFeeRate implements lndclient.WalletKitClient. Delegates
// to the test-configured override and records the target argument
// for assertion.
func (m *mockWalletKit) EstimateFeeRate(ctx context.Context, confTarget int32) (
	chainfee.SatPerKWeight, error) {

	m.gotTarget = confTarget

	if m.overrideEstimateFeeRate == nil {
		return chainfee.FeePerKwFloor, nil
	}

	return m.overrideEstimateFeeRate(ctx, confTarget)
}

// TestWalletKitEstimatorNilFallback asserts the constructor returns
// nil when the walletKit argument is nil. The caller at
// setupFeesSubsystem gates selection on a non-nil WalletKit, so a nil
// reaching the constructor is a wiring bug and the caller should
// crash rather than silently using a zero-value estimator.
func TestWalletKitEstimatorNilFallback(t *testing.T) {
	t.Parallel()

	est := NewWalletKitEstimator(nil, btclog.Disabled)
	require.Nil(t, est, "nil walletKit must return nil estimator")
}

// TestWalletKitEstimatorPassesThroughRate asserts a clean WalletKit
// response is returned verbatim with no transformation. This is the
// core #267 requirement: the operator's EstimateFee quote and
// validateOperatorFee both see the real chain rate, not the static
// FeePerKwFloor floor.
func TestWalletKitEstimatorPassesThroughRate(t *testing.T) {
	t.Parallel()

	const wantRate = chainfee.SatPerKWeight(7_500)

	mock := &mockWalletKit{
		overrideEstimateFeeRate: func(_ context.Context, _ int32) (
			chainfee.SatPerKWeight, error) {

			return wantRate, nil
		},
	}

	est := NewWalletKitEstimator(mock, btclog.Disabled)
	require.NotNil(t, est)

	got, err := est.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, wantRate, got)
	require.Equal(
		t, int32(6), mock.gotTarget,
		"confTarget must pass through unchanged",
	)
}

// TestWalletKitEstimatorFallbackOnError asserts an error from
// WalletKit is absorbed and the estimator returns
// chainfee.FeePerKwFloor with a nil error. The #267 design treats a
// transient chain-backend error as "quote conservatively" rather
// than surfacing the error all the way up to a client RPC.
func TestWalletKitEstimatorFallbackOnError(t *testing.T) {
	t.Parallel()

	mock := &mockWalletKit{
		overrideEstimateFeeRate: func(_ context.Context, _ int32) (
			chainfee.SatPerKWeight, error) {

			return 0, errors.New("backend offline")
		},
	}

	est := NewWalletKitEstimator(mock, btclog.Disabled)
	require.NotNil(t, est)

	got, err := est.EstimateFeePerKW(3)
	require.NoError(t, err,
		"backend errors must not propagate to callers")
	require.Equal(
		t, chainfee.FeePerKwFloor, got,
		"error path must return the conservative floor",
	)
}

// TestWalletKitEstimatorClampsConfTarget asserts that a confTarget
// above math.MaxInt32 clamps to MaxInt32 rather than silently
// wrapping to a negative. The WalletKit signature takes int32, so
// any value above 2^31-1 would sign-underflow through a naked cast.
// In practice the fee subsystem passes single-digit targets, but the
// clamp is defense-in-depth.
func TestWalletKitEstimatorClampsConfTarget(t *testing.T) {
	t.Parallel()

	mock := &mockWalletKit{}

	est := NewWalletKitEstimator(mock, btclog.Disabled)
	require.NotNil(t, est)

	// Pick a value above MaxInt32 to trigger the clamp branch.
	const oversized = uint32(math.MaxInt32) + 1

	_, err := est.EstimateFeePerKW(oversized)
	require.NoError(t, err)
	require.Equal(
		t, int32(math.MaxInt32), mock.gotTarget, "oversized "+
			"targets clamp to MaxInt32 rather than wrapping "+
			"negative",
	)
}

// TestWalletKitEstimatorStartStopNoop asserts Start and Stop are
// no-ops. The daemon's own lifecycle owns the underlying lndclient;
// the estimator has no background work to start or tear down.
func TestWalletKitEstimatorStartStopNoop(t *testing.T) {
	t.Parallel()

	est := NewWalletKitEstimator(
		&mockWalletKit{}, btclog.Disabled,
	)
	require.NoError(t, est.Start())
	require.NoError(t, est.Stop())
}

// TestWalletKitEstimatorRelayFee asserts RelayFeePerKW returns the
// chain-fee floor rather than hitting WalletKit for a second
// round-trip. The fee subsystem's validateOperatorFee compares
// derived rates against this floor; keeping it at FeePerKwFloor
// matches the previous static estimator behavior.
func TestWalletKitEstimatorRelayFee(t *testing.T) {
	t.Parallel()

	est := NewWalletKitEstimator(
		&mockWalletKit{}, btclog.Disabled,
	)
	require.Equal(
		t, chainfee.FeePerKwFloor, est.RelayFeePerKW(),
	)
}

// TestWalletKitEstimatorCachesLastSuccess asserts that after a
// successful EstimateFeeRate call, a subsequent error returns the
// cached rate rather than the bare FeePerKwFloor. This closes the
// silent-absorption hole the H-2 review finding flagged: returning
// 253 sat/kW on every error during a sustained LND outage would
// converge both quote and validation onto the floor while real
// mempool rates sat above it, making the operator eat the delta.
func TestWalletKitEstimatorCachesLastSuccess(t *testing.T) {
	t.Parallel()

	const cachedRate = chainfee.SatPerKWeight(8_000)

	var fail atomic.Bool
	mock := &mockWalletKit{
		overrideEstimateFeeRate: func(_ context.Context, _ int32) (
			chainfee.SatPerKWeight, error) {

			if fail.Load() {
				return 0, errors.New("backend offline")
			}

			return cachedRate, nil
		},
	}

	est := NewWalletKitEstimator(mock, btclog.Disabled)
	require.NotNil(t, est)

	// First call succeeds and seeds the cache.
	got, err := est.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, cachedRate, got)

	// Second call errors; fallback uses the cached rate, not the
	// bare floor.
	fail.Store(true)
	got, err = est.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(
		t, cachedRate, got, "error after a successful call must "+
			"return the cached rate, not the bare FeePerKwFloor",
	)
}

// TestWalletKitEstimatorClampsSubFloorCache asserts that even if a
// buggy WalletKit somehow seeded the cache with a sub-floor rate
// (which the success path itself prevents via the clamp at write
// time), a fallback read still returns at least FeePerKwFloor. This
// is belt-and-suspenders against any future code path that bypasses
// the clamp or any direct lastRate manipulation in tests.
func TestWalletKitEstimatorClampsSubFloorCache(t *testing.T) {
	t.Parallel()

	mock := &mockWalletKit{
		overrideEstimateFeeRate: func(_ context.Context, _ int32) (
			chainfee.SatPerKWeight, error) {

			return 0, errors.New("offline")
		},
	}

	est := NewWalletKitEstimator(mock, btclog.Disabled)
	require.NotNil(t, est)

	// Force a sub-floor cached rate directly (simulating a code
	// path that bypassed the success-side clamp). The fallback
	// must still floor it.
	est.mu.Lock()
	est.lastRate = chainfee.SatPerKWeight(50)
	est.mu.Unlock()

	got, err := est.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(
		t, chainfee.FeePerKwFloor, got,
		"sub-floor cached rate must clamp up to FeePerKwFloor",
	)
}

// TestWalletKitEstimatorClampsSuccessSubFloor asserts that a
// WalletKit response below FeePerKwFloor is clamped on the success
// path. The lndclient interface gives us no guarantee of a
// floor-respecting rate, and a zero/sub-floor rate from a buggy or
// compromised LND would zero out the on-chain fee share. The
// success-side clamp is the symmetric counterpart to the error-side
// floor in fallbackRate.
func TestWalletKitEstimatorClampsSuccessSubFloor(t *testing.T) {
	t.Parallel()

	mock := &mockWalletKit{
		overrideEstimateFeeRate: func(_ context.Context, _ int32) (
			chainfee.SatPerKWeight, error) {

			// Return a rate well below the relay floor.
			return chainfee.SatPerKWeight(50), nil
		},
	}

	est := NewWalletKitEstimator(mock, btclog.Disabled)
	require.NotNil(t, est)

	got, err := est.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(
		t, chainfee.FeePerKwFloor, got,
		"sub-floor success response must clamp up to FeePerKwFloor",
	)
}

// TestWalletKitEstimatorTimeout asserts that a hung WalletKit call
// is bounded by the per-call timeout and the timeout-derived error
// triggers the fallback path rather than blocking the caller
// forever. validateOperatorFee runs inside the per-round FSM event
// loop; a hung LND without this timeout would stall every join on
// that round.
//
// Uses NewWalletKitEstimatorWithTimeout to inject a 50ms ceiling so
// the test exercises the boundary without paying the production 15s
// per CI run.
func TestWalletKitEstimatorTimeout(t *testing.T) {
	t.Parallel()

	const testTimeout = 50 * time.Millisecond

	mock := &mockWalletKit{
		overrideEstimateFeeRate: func(ctx context.Context, _ int32) (
			chainfee.SatPerKWeight, error) {

			// Block until the per-call ctx is cancelled by
			// the timeout (or a generous deadline, as a
			// belt-and-suspenders).
			select {
			case <-ctx.Done():
				return 0, ctx.Err()

			case <-time.After(5 * time.Second):
				return 0, errors.New("test deadline")
			}
		},
	}

	est := NewWalletKitEstimatorWithTimeout(
		mock, btclog.Disabled, testTimeout,
	)
	require.NotNil(t, est)

	start := time.Now()
	got, err := est.EstimateFeePerKW(6)
	elapsed := time.Since(start)

	require.NoError(t, err,
		"timeout must trigger fallback, not propagate")
	require.Equal(
		t, chainfee.FeePerKwFloor, got,
		"first-call timeout (no cache) must return FeePerKwFloor",
	)
	require.LessOrEqual(
		t, elapsed, testTimeout*10,
		"call must return within ~timeout, not block indefinitely",
	)
}
