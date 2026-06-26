package chainfees

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const defaultWalletKitEstimateTimeout = 15 * time.Second

// WalletKitEstimator implements chainfee.Estimator by proxying fee estimates
// to an lndclient WalletKitClient.
type WalletKitEstimator struct {
	walletKit lndclient.WalletKitClient
	log       btclog.Logger

	estimateTimeout time.Duration
	fallbackOnError bool

	mu       sync.Mutex
	lastRate chainfee.SatPerKWeight
}

// WalletKitEstimatorConfig configures a WalletKitEstimator.
type WalletKitEstimatorConfig struct {
	// WalletKit is the lndclient WalletKit client used for fee estimates.
	WalletKit lndclient.WalletKitClient

	// Log is an optional structured logger.
	Log btclog.Logger

	// Timeout bounds each WalletKit fee estimate call.
	Timeout time.Duration

	// FallbackOnError makes WalletKit errors return the last successful
	// rate, or the relay floor before any successful estimate. It defaults
	// to disabled (fail-fast): errors propagate to the caller. Only enable
	// it for a standalone estimator that prefers a stale rate over a hard
	// failure; never enable it when composing WalletKit inside a selector,
	// where a stale/floor value could beat another live provider.
	FallbackOnError bool
}

// NewWalletKitEstimator builds a fail-fast WalletKitEstimator backed by
// walletKit: WalletKit errors propagate to the caller. This is the safe
// default for composing WalletKit inside a selector. Use
// NewFallbackWalletKitEstimator for a standalone estimator that should serve a
// stale rate instead of failing.
func NewWalletKitEstimator(walletKit lndclient.WalletKitClient,
	log btclog.Logger) (*WalletKitEstimator, error) {

	return NewWalletKitEstimatorWithConfig(WalletKitEstimatorConfig{
		WalletKit: walletKit,
		Log:       log,
	})
}

// NewWalletKitEstimatorWithTimeout builds a fail-fast WalletKitEstimator with a
// custom per-call timeout.
func NewWalletKitEstimatorWithTimeout(walletKit lndclient.WalletKitClient,
	log btclog.Logger, timeout time.Duration) (*WalletKitEstimator, error) {

	return NewWalletKitEstimatorWithConfig(WalletKitEstimatorConfig{
		WalletKit: walletKit,
		Log:       log,
		Timeout:   timeout,
	})
}

// NewFallbackWalletKitEstimator builds a WalletKitEstimator that returns the
// last successful rate (or the relay floor before any success) instead of
// propagating WalletKit errors. Use this only for a standalone estimator;
// never compose it inside a selector, where a stale fallback could beat
// another live provider.
func NewFallbackWalletKitEstimator(walletKit lndclient.WalletKitClient,
	log btclog.Logger) (*WalletKitEstimator, error) {

	return NewWalletKitEstimatorWithConfig(WalletKitEstimatorConfig{
		WalletKit:       walletKit,
		Log:             log,
		FallbackOnError: true,
	})
}

// NewWalletKitEstimatorWithConfig builds a WalletKitEstimator from cfg. It
// returns an error when the WalletKit client is missing, rather than a typed
// nil pointer that would panic on first use once boxed into an interface.
func NewWalletKitEstimatorWithConfig(cfg WalletKitEstimatorConfig) (
	*WalletKitEstimator, error) {

	if cfg.WalletKit == nil {
		return nil, fmt.Errorf("walletkit client is required")
	}

	log := cfg.Log
	if log == nil {
		log = btclog.Disabled
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultWalletKitEstimateTimeout
	}

	return &WalletKitEstimator{
		walletKit:       cfg.WalletKit,
		log:             log,
		estimateTimeout: timeout,
		fallbackOnError: cfg.FallbackOnError,
	}, nil
}

// EstimateFeePerKW returns the current chain fee rate for the given
// confirmation target, in sat/kW.
func (e *WalletKitEstimator) EstimateFeePerKW(confTarget uint32) (
	chainfee.SatPerKWeight, error) {

	target := int32(confTarget)
	if confTarget > math.MaxInt32 {
		target = math.MaxInt32
	}

	ctx, cancel := context.WithTimeout(
		context.Background(), e.estimateTimeout,
	)
	defer cancel()

	rate, err := e.walletKit.EstimateFeeRate(ctx, target)
	if err != nil {
		if !e.fallbackOnError {
			return 0, fmt.Errorf("estimate walletkit fee rate: %w",
				err)
		}

		fallback := e.fallbackRate()
		e.log.WarnS(
			context.Background(),
			"WalletKit EstimateFeeRate failed; falling back "+
				"to last successful rate",
			err,
			slog.Int64("fallback_sat_kw", int64(fallback)),
		)

		return fallback, nil
	}

	if rate < chainfee.FeePerKwFloor {
		e.log.WarnS(
			context.Background(),
			"WalletKit returned sub-floor rate; clamping",
			fmt.Errorf("rate %d below floor %d", rate,
				chainfee.FeePerKwFloor),
			slog.Int64("raw_sat_kw", int64(rate)),
		)
		rate = chainfee.FeePerKwFloor
	}

	e.log.DebugS(
		context.Background(),
		"WalletKit EstimateFeeRate succeeded",
		slog.Uint64("conf_target", uint64(target)),
		slog.Int64("rate_sat_kw", int64(rate)),
		slog.Int64("rate_sat_vbyte", int64(rate.FeePerVByte())),
	)

	e.mu.Lock()
	e.lastRate = rate
	e.mu.Unlock()

	return rate, nil
}

// Start is a no-op. The backing lndclient owns its own lifecycle.
func (e *WalletKitEstimator) Start() error { return nil }

// Stop is a no-op. The backing lndclient owns its own lifecycle.
func (e *WalletKitEstimator) Stop() error { return nil }

// RelayFeePerKW returns the floor relay fee.
func (e *WalletKitEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	return chainfee.FeePerKwFloor
}

// fallbackRate returns the last successful WalletKit rate, clamped to the relay
// floor.
func (e *WalletKitEstimator) fallbackRate() chainfee.SatPerKWeight {
	e.mu.Lock()
	cached := e.lastRate
	e.mu.Unlock()

	if cached < chainfee.FeePerKwFloor {
		return chainfee.FeePerKwFloor
	}

	return cached
}

var _ chainfee.Estimator = (*WalletKitEstimator)(nil)
