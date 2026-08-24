package chainfees

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// BackendEstimator adapts Wavelength's chain fee source to lnd's channel fee
// estimator interface.
type BackendEstimator struct {
	backend  chainsource.ChainBackend
	relayFee chainfee.SatPerKWeight
}

// NewBackendEstimator constructs a live estimator with a fixed relay floor.
func NewBackendEstimator(backend chainsource.ChainBackend,
	relayFee chainfee.SatPerKWeight) (*BackendEstimator, error) {

	if backend == nil {
		return nil, fmt.Errorf("chain backend is required")
	}
	if relayFee < chainfee.FeePerKwFloor {
		relayFee = chainfee.FeePerKwFloor
	}

	return &BackendEstimator{
		backend: backend, relayFee: relayFee,
	}, nil
}

// EstimateFeePerKW obtains a current sat/vbyte estimate and converts it to
// lnd's sat/kw unit while enforcing the relay floor.
func (e *BackendEstimator) EstimateFeePerKW(confTarget uint32) (
	chainfee.SatPerKWeight, error) {

	rate, err := e.backend.EstimateFee(context.Background(), confTarget)
	if err != nil {

		// A fee-source outage must not disable channel operation. The
		// configured relay floor is a conservative usable estimate.
		//nolint:nilerr
		return e.relayFee, nil
	}
	if rate <= 0 {
		return e.relayFee, nil
	}
	feeRate := chainfee.SatPerVByte(rate).FeePerKWeight()
	if feeRate < e.relayFee {
		feeRate = e.relayFee
	}

	return feeRate, nil
}

// RelayFeePerKW returns the configured minimum relay fee.
func (e *BackendEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	return e.relayFee
}

// Start leaves lifecycle ownership with Wavelength's shared chain backend.
func (*BackendEstimator) Start() error {
	return nil
}

// Stop leaves lifecycle ownership with Wavelength's shared chain backend.
func (*BackendEstimator) Stop() error {
	return nil
}

var _ chainfee.Estimator = (*BackendEstimator)(nil)
