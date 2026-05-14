package chainfees

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const minEstimatorDivergenceWarnPct = 20

// NamedEstimator gives a child estimator a stable name for logs.
type NamedEstimator struct {
	// Name identifies the estimator in logs.
	Name string

	// Estimator is the wrapped fee estimator.
	Estimator chainfee.Estimator
}

// MinEstimator queries multiple fee estimators and returns the minimum
// successful estimate.
type MinEstimator struct {
	log      btclog.Logger
	children []NamedEstimator

	mu       sync.Mutex
	lastRate chainfee.SatPerKWeight
}

// NewMinEstimator constructs a chainfee.Estimator that chooses the lowest
// successful estimate from children. Children should propagate backend errors
// instead of returning stale fallback rates, otherwise a fallback floor can
// incorrectly win over another provider's live estimate. Use
// NewFailFastWalletKitEstimator when composing WalletKit in this selector.
func NewMinEstimator(log btclog.Logger,
	children ...NamedEstimator) (*MinEstimator, error) {

	if log == nil {
		log = btclog.Disabled
	}

	if len(children) == 0 {
		return nil, fmt.Errorf("at least one fee estimator is required")
	}

	for idx, child := range children {
		if child.Name == "" {
			return nil, fmt.Errorf("fee estimator %d has "+
				"empty name", idx)
		}
		if child.Estimator == nil {
			return nil, fmt.Errorf("fee estimator %q is nil",
				child.Name)
		}
	}

	return &MinEstimator{
		log:      log,
		children: children,
	}, nil
}

// EstimateFeePerKW returns the minimum successful estimate for confTarget.
func (e *MinEstimator) EstimateFeePerKW(confTarget uint32) (
	chainfee.SatPerKWeight, error) {

	var (
		selectedName string
		selectedRate chainfee.SatPerKWeight
		maxRate      chainfee.SatPerKWeight
		found        bool
		lastErr      error
	)

	for _, child := range e.children {
		rate, err := child.Estimator.EstimateFeePerKW(confTarget)
		if err != nil {
			lastErr = err
			e.log.WarnS(
				context.Background(),
				"Fee provider estimate failed",
				err,
				slog.String("provider", child.Name),
				slog.Uint64("conf_target", uint64(confTarget)),
			)

			continue
		}

		if rate < chainfee.FeePerKwFloor {
			e.log.WarnS(context.Background(),
				"Fee provider returned sub-floor rate; "+
					"clamping",
				fmt.Errorf("rate %d below floor %d", rate,
					chainfee.FeePerKwFloor),
				slog.String("provider", child.Name),
				slog.Int64("raw_sat_kw", int64(rate)),
			)
			rate = chainfee.FeePerKwFloor
		}

		e.log.DebugS(context.Background(), "Fee provider estimate",
			slog.String("provider", child.Name),
			slog.Uint64("conf_target", uint64(confTarget)),
			slog.Int64("rate_sat_kw", int64(rate)),
			slog.Int64("rate_sat_vbyte", int64(rate.FeePerVByte())),
		)

		if !found || rate < selectedRate {
			selectedName = child.Name
			selectedRate = rate
		}
		if !found || rate > maxRate {
			maxRate = rate
		}
		found = true
	}

	if !found {
		fallback := e.fallbackRate()
		e.log.WarnS(context.Background(),
			"All fee providers failed; falling back to last "+
				"successful rate",
			lastErr,
			slog.Uint64("conf_target", uint64(confTarget)),
			slog.Int64("fallback_sat_kw", int64(fallback)),
		)

		return fallback, nil
	}

	if estimateDiverges(selectedRate, maxRate) {
		e.log.InfoS(
			context.Background(),
			"Fee provider estimates diverged",
			slog.Uint64("conf_target", uint64(confTarget)),
			slog.Int64("min_sat_kw", int64(selectedRate)),
			slog.Int64("max_sat_kw", int64(maxRate)),
		)
	}

	e.log.InfoS(context.Background(), "Fee estimator selected provider",
		slog.String("strategy", "min"),
		slog.String("provider", selectedName),
		slog.Uint64("conf_target", uint64(confTarget)),
		slog.Int64("rate_sat_kw", int64(selectedRate)),
		slog.Int64("rate_sat_vbyte", int64(selectedRate.FeePerVByte())),
	)

	e.mu.Lock()
	e.lastRate = selectedRate
	e.mu.Unlock()

	return selectedRate, nil
}

// Start starts all child estimators.
func (e *MinEstimator) Start() error {
	var started []NamedEstimator
	for _, child := range e.children {
		if err := child.Estimator.Start(); err != nil {
			startErr := fmt.Errorf("start fee estimator %q: %w",
				child.Name, err)

			return errors.Join(startErr, stopEstimators(started))
		}

		started = append(started, child)
	}

	return nil
}

// Stop stops all child estimators.
func (e *MinEstimator) Stop() error {
	var stopErr error
	for _, child := range e.children {
		if err := child.Estimator.Stop(); err != nil {
			stopErr = errors.Join(
				stopErr, fmt.Errorf("stop fee estimator "+
					"%q: %w", child.Name, err),
			)
		}
	}

	return stopErr
}

// RelayFeePerKW returns the highest relay fee floor reported by a child.
func (e *MinEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	relayFee := chainfee.FeePerKwFloor
	for _, child := range e.children {
		childRelayFee := child.Estimator.RelayFeePerKW()
		if childRelayFee > relayFee {
			relayFee = childRelayFee
		}
	}

	return relayFee
}

func (e *MinEstimator) fallbackRate() chainfee.SatPerKWeight {
	e.mu.Lock()
	cached := e.lastRate
	e.mu.Unlock()

	if cached < chainfee.FeePerKwFloor {
		return chainfee.FeePerKwFloor
	}

	return cached
}

func stopEstimators(children []NamedEstimator) error {
	var stopErr error
	for i := len(children) - 1; i >= 0; i-- {
		child := children[i]
		if err := child.Estimator.Stop(); err != nil {
			stopErr = errors.Join(
				stopErr, fmt.Errorf("stop fee estimator %q "+
					"after start failure: %w", child.Name,
					err),
			)
		}
	}

	return stopErr
}

func estimateDiverges(minRate, maxRate chainfee.SatPerKWeight) bool {
	if minRate <= 0 || maxRate <= minRate {
		return false
	}

	threshold := minRate + (minRate * minEstimatorDivergenceWarnPct / 100)

	return maxRate > threshold
}

var _ chainfee.Estimator = (*MinEstimator)(nil)
