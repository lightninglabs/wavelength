package lndbackend

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// defaultWalletKitEstimateTimeout is the production per-call ceiling
// on how long we will block waiting for WalletKit.EstimateFeeRate to
// return. The chainfee.Estimator interface gives us no caller-
// supplied context (Start/Stop/EstimateFeePerKW take no ctx), so we
// have to bound the call ourselves; otherwise a hung LND would stall
// every EstimateFee RPC and every per-join validateOperatorFee
// inside the round FSM event loop. Fifteen seconds matches the
// existing timeout the client side already uses in
// chainbackends/lndclient_adapters.go.
//
// The constructor accepts an override so unit tests can drop this to
// sub-millisecond and exercise the timeout path without paying real
// wall-clock seconds per CI run.
const defaultWalletKitEstimateTimeout = 15 * time.Second

// WalletKitEstimator implements chainfee.Estimator by proxying every
// EstimateFeePerKW call to the backing lndclient.WalletKitClient. It
// replaces the previous setupFeesSubsystem hardcoded static floor
// estimator so both the EstimateFee quote surface and validateOperatorFee
// see live mempool rates.
//
// Under a mempool spike the operator was previously eating the delta
// between the 253 sat/kW static floor and the true chain cost because
// both the quote and the validation drew from the same static source;
// switching to WalletKit closes that silent-absorption path. There is no
// local conservative pad: issue #270's seal-time fee handshake removes
// the quote-time vs seal-time drift by construction, so the interim
// (#267) only needs to report whatever WalletKit reports right now.
//
// On any WalletKit error the estimator falls back to the last
// successfully observed rate (clamped to chainfee.FeePerKwFloor),
// rather than to the floor itself. Returning the bare floor on every
// error would re-open the silent-absorption hole #267 closes: a
// sustained LND outage would converge both quote and validation onto
// 253 sat/kW while the real mempool sat above it, and the operator
// would silently eat the delta. Keeping the last good rate keeps the
// fee floor anchored to recent reality even while WalletKit is
// unavailable. The first error before any successful call still
// falls all the way back to FeePerKwFloor — there is nothing better
// to fall back to until WalletKit responds at least once.
type WalletKitEstimator struct {
	walletKit lndclient.WalletKitClient

	// log is the package logger. Never nil: the constructor falls
	// back to btclog.Disabled when the caller passes nil.
	log btclog.Logger

	// estimateTimeout is the per-call ceiling on the WalletKit
	// EstimateFeeRate RPC. Defaults to
	// defaultWalletKitEstimateTimeout in production; tests inject a
	// shorter value via NewWalletKitEstimatorWithTimeout so the
	// timeout-fallback path can be exercised without paying real
	// wall-clock seconds per CI run.
	estimateTimeout time.Duration

	// mu guards lastRate. Reads + writes happen on whichever
	// goroutine the chainfee.Estimator caller is on (EstimateFee
	// RPC handler, rounds FSM event loop, etc), so we need a real
	// lock rather than just a relaxed atomic.
	mu sync.Mutex

	// lastRate caches the most recent successful EstimateFeeRate
	// response. Zero means "no successful response observed yet"
	// and triggers the bare FeePerKwFloor fallback. Updated under
	// mu after every successful WalletKit call; read under mu on
	// every error fallback. The clamp to FeePerKwFloor lives at
	// the read site so a buggy WalletKit returning a sub-floor
	// rate cannot silently poison the cache.
	lastRate chainfee.SatPerKWeight
}

// NewWalletKitEstimator builds a WalletKitEstimator backed by the given
// lndclient. The walletKit argument must be non-nil; the caller is
// expected to gate selection in setupFeesSubsystem so this constructor
// is only reached when an LND connection is available. Passing nil
// returns nil so a mis-wired call site fails obviously at estimator
// first-use rather than silently reporting the static floor forever.
//
// The log argument may be nil; in that case btclog.Disabled is used so
// the fallback-on-error path still has a valid logger target.
func NewWalletKitEstimator(walletKit lndclient.WalletKitClient,
	log btclog.Logger) *WalletKitEstimator {

	return NewWalletKitEstimatorWithTimeout(
		walletKit, log, defaultWalletKitEstimateTimeout,
	)
}

// NewWalletKitEstimatorWithTimeout is the test-injectable form of
// NewWalletKitEstimator. Production callers should use the
// no-timeout-arg constructor; tests pass a sub-millisecond timeout
// to exercise the timeout-fallback path without blocking the test
// suite for the production 15-second ceiling.
//
// A non-positive timeout is replaced with
// defaultWalletKitEstimateTimeout so a misconfigured caller still
// gets the safe production default.
func NewWalletKitEstimatorWithTimeout(walletKit lndclient.WalletKitClient,
	log btclog.Logger, timeout time.Duration) *WalletKitEstimator {

	if walletKit == nil {
		return nil
	}

	if log == nil {
		log = btclog.Disabled
	}

	if timeout <= 0 {
		timeout = defaultWalletKitEstimateTimeout
	}

	return &WalletKitEstimator{
		walletKit:       walletKit,
		log:             log,
		estimateTimeout: timeout,
	}
}

// EstimateFeePerKW returns the current chain fee rate for the given
// confirmation target, in sat/kW. On any backend error the method
// logs and returns the last successful rate (clamped to
// chainfee.FeePerKwFloor); if there has been no successful call yet
// it returns FeePerKwFloor. The fee subsystem keeps running at a
// conservative rate instead of surfacing the RPC error to every
// caller.
//
// confTarget is the number of blocks we want the tx to confirm within,
// matching the chainfee.Estimator contract. WalletKit.EstimateFeeRate
// takes int32 rather than uint32, so very large targets are clamped to
// math.MaxInt32 to keep the cast safe; in practice the call site
// passes Rounds.ConfTarget which is in single-digit territory.
func (e *WalletKitEstimator) EstimateFeePerKW(
	confTarget uint32) (chainfee.SatPerKWeight, error) {

	// Clamp to int32 for the WalletKit signature. Any target above
	// int32 range is nonsense for a chain fee estimator anyway, so a
	// silent clamp is fine.
	target := int32(confTarget)
	if confTarget > math.MaxInt32 {
		target = math.MaxInt32
	}

	// Bound the WalletKit call so a hung LND cannot stall the
	// caller. The chainfee.Estimator interface gives us no
	// upstream context, so the timeout is the only safety belt.
	ctx, cancel := context.WithTimeout(
		context.Background(), e.estimateTimeout,
	)
	defer cancel()

	rate, err := e.walletKit.EstimateFeeRate(ctx, target)
	if err != nil {
		fallback := e.fallbackRate()
		e.log.WarnS(
			context.Background(),
			"WalletKit EstimateFeeRate failed; falling back "+
				"to last successful rate",
			err,
			"fallback_sat_kw", int64(fallback),
		)

		return fallback, nil
	}

	// Floor-clamp on the success path too: the lndclient interface
	// makes no guarantee about a sub-floor response, and a
	// zero-valued rate from a buggy or compromised LND would zero
	// out the on-chain fee share entirely. Symmetric with the
	// error path which already returns at least FeePerKwFloor.
	if rate < chainfee.FeePerKwFloor {
		e.log.WarnS(
			context.Background(),
			"WalletKit returned sub-floor rate; clamping",
			fmt.Errorf("rate %d below floor %d",
				rate, chainfee.FeePerKwFloor),
			"raw_sat_kw", int64(rate),
		)
		rate = chainfee.FeePerKwFloor
	}

	e.mu.Lock()
	e.lastRate = rate
	e.mu.Unlock()

	return rate, nil
}

// fallbackRate returns the rate to use when WalletKit is unavailable.
// Always at least chainfee.FeePerKwFloor so a never-yet-succeeded
// estimator (lastRate = 0) still produces a valid relay-floor result;
// otherwise the last successful rate, which keeps the fee floor
// anchored to recent reality during a transient LND outage rather
// than collapsing all the way to FeePerKwFloor.
func (e *WalletKitEstimator) fallbackRate() chainfee.SatPerKWeight {
	e.mu.Lock()
	cached := e.lastRate
	e.mu.Unlock()

	if cached < chainfee.FeePerKwFloor {
		return chainfee.FeePerKwFloor
	}

	return cached
}

// Start is a no-op. The backing lndclient is started by the daemon's
// own lifecycle; nothing to do here.
func (e *WalletKitEstimator) Start() error { return nil }

// Stop is a no-op for the same reason as Start.
func (e *WalletKitEstimator) Stop() error { return nil }

// RelayFeePerKW returns the floor relay fee. Most callers of this
// method in the darepo fee subsystem compare the derived fee rate
// against this floor, so keeping it at FeePerKwFloor matches the
// previous static estimator behavior and avoids a second WalletKit
// round-trip on every validation call.
func (e *WalletKitEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	return chainfee.FeePerKwFloor
}

// compile-time assertion that WalletKitEstimator satisfies
// chainfee.Estimator.
var _ chainfee.Estimator = (*WalletKitEstimator)(nil)
