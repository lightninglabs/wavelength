package chainfees

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

type testEstimator struct {
	rate      chainfee.SatPerKWeight
	relayFee  chainfee.SatPerKWeight
	err       error
	started   bool
	stopped   bool
	gotTarget uint32
	startErr  error
	stopErr   error
}

func (e *testEstimator) EstimateFeePerKW(target uint32) (chainfee.SatPerKWeight,
	error) {

	e.gotTarget = target

	if e.err != nil {
		return 0, e.err
	}

	return e.rate, nil
}

func (e *testEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	return e.relayFee
}

func (e *testEstimator) Start() error {
	e.started = true

	return e.startErr
}

func (e *testEstimator) Stop() error {
	e.stopped = true

	return e.stopErr
}

func TestMinEstimatorChoosesLowestSuccessfulRate(t *testing.T) {
	t.Parallel()

	high := &testEstimator{
		rate: chainfee.SatPerKWeight(10_000),
	}
	low := &testEstimator{
		rate: chainfee.SatPerKWeight(500),
	}

	estimator, err := NewMinEstimator(nil,
		NamedEstimator{
			Name:      "walletkit",
			Estimator: high,
		},
		NamedEstimator{
			Name:      "mempool-space",
			Estimator: low,
		},
	)
	require.NoError(t, err)

	got, err := estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, low.rate, got)
	require.Equal(t, uint32(6), high.gotTarget)
	require.Equal(t, uint32(6), low.gotTarget)
}

func TestMinEstimatorSkipsFailedProvider(t *testing.T) {
	t.Parallel()

	good := &testEstimator{
		rate: chainfee.SatPerKWeight(750),
	}

	estimator, err := NewMinEstimator(nil,
		NamedEstimator{
			Name: "broken",
			Estimator: &testEstimator{
				err: fmt.Errorf("boom"),
			},
		},
		NamedEstimator{
			Name:      "good",
			Estimator: good,
		},
	)
	require.NoError(t, err)

	got, err := estimator.EstimateFeePerKW(3)
	require.NoError(t, err)
	require.Equal(t, good.rate, got)
}

func TestMinEstimatorFallsBackToLastGoodRate(t *testing.T) {
	t.Parallel()

	child := &testEstimator{
		rate: chainfee.SatPerKWeight(900),
	}
	estimator, err := NewMinEstimator(nil, NamedEstimator{
		Name:      "child",
		Estimator: child,
	})
	require.NoError(t, err)

	got, err := estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, child.rate, got)

	child.err = fmt.Errorf("offline")
	got, err = estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, child.rate, got)
}

func TestMinEstimatorFallbackStartsAtFloor(t *testing.T) {
	t.Parallel()

	estimator, err := NewMinEstimator(nil, NamedEstimator{
		Name: "broken",
		Estimator: &testEstimator{
			err: fmt.Errorf("offline"),
		},
	})
	require.NoError(t, err)

	got, err := estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, chainfee.FeePerKwFloor, got)
}

func TestMinEstimatorRelayFeeUsesHighestFloor(t *testing.T) {
	t.Parallel()

	estimator, err := NewMinEstimator(nil,
		NamedEstimator{
			Name: "low",
			Estimator: &testEstimator{
				relayFee: chainfee.SatPerKWeight(300),
			},
		},
		NamedEstimator{
			Name: "high",
			Estimator: &testEstimator{
				relayFee: chainfee.SatPerKWeight(1_000),
			},
		},
	)
	require.NoError(t, err)

	require.Equal(
		t, chainfee.SatPerKWeight(1_000), estimator.RelayFeePerKW(),
	)
}

func TestMinEstimatorStartsAndStopsChildren(t *testing.T) {
	t.Parallel()

	child := &testEstimator{}
	estimator, err := NewMinEstimator(nil, NamedEstimator{
		Name:      "child",
		Estimator: child,
	})
	require.NoError(t, err)

	require.NoError(t, estimator.Start())
	require.True(t, child.started)

	require.NoError(t, estimator.Stop())
	require.True(t, child.stopped)
}

func TestMinEstimatorStopsStartedChildrenAfterStartFailure(t *testing.T) {
	t.Parallel()

	started := &testEstimator{}
	failing := &testEstimator{
		startErr: fmt.Errorf("start failed"),
	}
	unstarted := &testEstimator{}
	estimator, err := NewMinEstimator(nil,
		NamedEstimator{
			Name:      "started",
			Estimator: started,
		},
		NamedEstimator{
			Name:      "failing",
			Estimator: failing,
		},
		NamedEstimator{
			Name:      "unstarted",
			Estimator: unstarted,
		},
	)
	require.NoError(t, err)

	err = estimator.Start()
	require.ErrorContains(t, err, "start fee estimator")
	require.True(t, started.started)
	require.True(t, started.stopped)
	require.True(t, failing.started)
	require.False(t, failing.stopped)
	require.False(t, unstarted.started)
	require.False(t, unstarted.stopped)
}

func TestMinEstimatorStopAggregatesErrors(t *testing.T) {
	t.Parallel()

	errOne := fmt.Errorf("one")
	errTwo := fmt.Errorf("two")
	first := &testEstimator{stopErr: errOne}
	second := &testEstimator{stopErr: errTwo}
	estimator, err := NewMinEstimator(nil,
		NamedEstimator{
			Name:      "first",
			Estimator: first,
		},
		NamedEstimator{
			Name:      "second",
			Estimator: second,
		},
	)
	require.NoError(t, err)

	err = estimator.Stop()
	require.Error(t, err)
	require.True(t, errors.Is(err, errOne))
	require.True(t, errors.Is(err, errTwo))
	require.True(t, first.stopped)
	require.True(t, second.stopped)
}

func TestEstimateDiverges(t *testing.T) {
	t.Parallel()

	require.False(
		t,
		estimateDiverges(
			chainfee.SatPerKWeight(1_000),
			chainfee.SatPerKWeight(1_200),
		),
	)
	require.True(
		t,
		estimateDiverges(
			chainfee.SatPerKWeight(1_000),
			chainfee.SatPerKWeight(1_201),
		),
	)
}
