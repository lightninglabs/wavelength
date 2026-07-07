package chainbackends

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

type stubNotifier struct {
	confEvent  *chainntnfs.ConfirmationEvent
	spendEvent *chainntnfs.SpendEvent
}

func (n *stubNotifier) RegisterConfirmationsNtfn(_ *chainhash.Hash, _ []byte, _,
	_ uint32, _ ...chainntnfs.NotifierOption) (
	*chainntnfs.ConfirmationEvent, error) {

	return n.confEvent, nil
}

func (n *stubNotifier) RegisterSpendNtfn(_ *wire.OutPoint, _ []byte, _ uint32) (
	*chainntnfs.SpendEvent, error) {

	return n.spendEvent, nil
}

func (n *stubNotifier) RegisterBlockEpochNtfn(_ *chainntnfs.BlockEpoch) (
	*chainntnfs.BlockEpochEvent, error) {

	return &chainntnfs.BlockEpochEvent{
		Epochs: make(chan *chainntnfs.BlockEpoch),
		Cancel: func() {},
	}, nil
}

func (n *stubNotifier) Start() error { return nil }

func (n *stubNotifier) Started() bool { return true }

func (n *stubNotifier) Stop() error { return nil }

type stubFeeEstimator struct {
	rate      chainfee.SatPerKWeight
	gotTarget uint32
}

func (s *stubFeeEstimator) EstimateFeePerKW(target uint32) (
	chainfee.SatPerKWeight, error) {

	s.gotTarget = target

	return s.rate, nil
}

func (s *stubFeeEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	return 0
}

func (s *stubFeeEstimator) Start() error { return nil }

func (s *stubFeeEstimator) Stop() error { return nil }

type stubWalletKitFeeEstimator struct {
	lndclient.WalletKitClient

	rate      chainfee.SatPerKWeight
	gotTarget int32
}

func (s *stubWalletKitFeeEstimator) EstimateFeeRate(_ context.Context,
	target int32) (chainfee.SatPerKWeight, error) {

	s.gotTarget = target

	return s.rate, nil
}

type stubBroadcaster struct{}

func (s *stubBroadcaster) PublishTransaction(
	_ context.Context, _ *wire.MsgTx, _ string,
) error {

	return nil
}

type stubPackageSubmitter struct {
	result *btcjson.SubmitPackageResult
	err    error
}

func (s *stubPackageSubmitter) SubmitPackage(_ context.Context,
	parents []*wire.MsgTx, child *wire.MsgTx, maxFeeRate *float64) (
	*btcjson.SubmitPackageResult, error) {

	return s.result, s.err
}

func TestLndClientFeeEstimatorReturnsWalletKitSatPerKW(t *testing.T) {
	t.Parallel()

	const wantRate = chainfee.SatPerKWeight(1_250)

	walletKit := &stubWalletKitFeeEstimator{
		rate: wantRate,
	}
	estimator, err := NewLndClientFeeEstimator(walletKit)
	require.NoError(t, err)

	gotRate, err := estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, wantRate, gotRate)
	require.Equal(t, int32(6), walletKit.gotTarget)
}

func TestLNDBackendEstimateFeeConvertsSatPerKWOnce(t *testing.T) {
	t.Parallel()

	const walletKitRate = chainfee.SatPerKWeight(31_774)

	estimator := &stubFeeEstimator{
		rate: walletKitRate,
	}
	backend := NewLNDBackend(
		&stubNotifier{}, estimator, &stubBroadcaster{},
	)

	gotRate, err := backend.EstimateFee(t.Context(), 6)
	require.NoError(t, err)
	require.Equal(t, int64(walletKitRate.FeePerVByte()), int64(gotRate))
	require.Equal(t, uint32(6), estimator.gotTarget)
}

func TestRegisterConfSurvivesCallerContextCancellation(t *testing.T) {
	t.Parallel()

	confChan := make(chan *chainntnfs.TxConfirmation, 1)
	notifier := &stubNotifier{
		confEvent: &chainntnfs.ConfirmationEvent{
			Confirmed: confChan,
			Cancel:    func() {},
			Done:      make(chan struct{}),
		},
	}
	backend := NewLNDBackend(
		notifier, &stubFeeEstimator{}, &stubBroadcaster{},
	)

	ctx, cancel := context.WithCancel(t.Context())
	reg, err := backend.RegisterConf(
		ctx, &chainhash.Hash{1}, []byte{0x51}, 1, 100, false,
	)
	require.NoError(t, err)

	cancel()

	expectedHash := chainhash.Hash{2}
	confChan <- &chainntnfs.TxConfirmation{
		BlockHash:   &expectedHash,
		BlockHeight: 123,
		Tx:          wire.NewMsgTx(2),
	}

	var got *chainsource.TxConfirmation
	require.Eventually(t, func() bool {
		select {
		case conf, ok := <-reg.Confirmed:
			if !ok {
				return false
			}

			got = conf

			return true

		default:
			return false
		}
	}, testTimeout, pollInterval)

	require.NotNil(t, got)
	require.Equal(t, uint32(123), got.BlockHeight)
	require.Equal(t, expectedHash, *got.BlockHash)
	reg.Cancel()
}

func TestRegisterSpendSurvivesCallerContextCancellation(t *testing.T) {
	t.Parallel()

	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	notifier := &stubNotifier{
		spendEvent: &chainntnfs.SpendEvent{
			Spend:  spendChan,
			Reorg:  make(chan struct{}),
			Done:   make(chan struct{}),
			Cancel: func() {},
		},
	}
	backend := NewLNDBackend(
		notifier, &stubFeeEstimator{}, &stubBroadcaster{},
	)

	ctx, cancel := context.WithCancel(t.Context())
	outpoint := &wire.OutPoint{Index: 1}
	reg, err := backend.RegisterSpend(ctx, outpoint, []byte{0x51}, 100)
	require.NoError(t, err)

	cancel()

	spenderHash := chainhash.Hash{3}
	spendChan <- &chainntnfs.SpendDetail{
		SpentOutPoint:  outpoint,
		SpenderTxHash:  &spenderHash,
		SpendingTx:     wire.NewMsgTx(2),
		SpendingHeight: 144,
	}

	var got *chainsource.SpendDetail
	require.Eventually(t, func() bool {
		select {
		case spend, ok := <-reg.Spend:
			if !ok {
				return false
			}

			got = spend

			return true

		default:
			return false
		}
	}, testTimeout, pollInterval)

	require.NotNil(t, got)
	require.Equal(t, int32(144), got.SpendingHeight)
	require.Equal(t, spenderHash, *got.SpenderTxHash)
	reg.Cancel()
}

func TestSubmitPackageUnsupported(t *testing.T) {
	t.Parallel()

	backend := NewLNDBackend(
		&stubNotifier{}, &stubFeeEstimator{}, &stubBroadcaster{},
	)

	err := backend.SubmitPackage(
		t.Context(), []*wire.MsgTx{wire.NewMsgTx(3)}, wire.NewMsgTx(3),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not supported")
}

func TestSubmitPackageSuccess(t *testing.T) {
	t.Parallel()

	backend := NewLNDBackend(
		&stubNotifier{}, &stubFeeEstimator{}, &stubBroadcaster{},
	)
	backend.SetPackageSubmitter(&stubPackageSubmitter{
		result: &btcjson.SubmitPackageResult{
			PackageMsg: "success",
			TxResults: map[string]btcjson.SubmitPackageTxResult{
				"wtxid-1": {
					TxID: chainhash.Hash{1},
				},
			},
		},
	})

	err := backend.SubmitPackage(
		t.Context(), []*wire.MsgTx{wire.NewMsgTx(3)}, wire.NewMsgTx(3),
	)
	require.NoError(t, err)
}

func TestSubmitPackageRejectsRejectedTransactions(t *testing.T) {
	t.Parallel()

	rejectReason := "insufficient fee"
	backend := NewLNDBackend(
		&stubNotifier{}, &stubFeeEstimator{}, &stubBroadcaster{},
	)
	backend.SetPackageSubmitter(&stubPackageSubmitter{
		result: &btcjson.SubmitPackageResult{
			PackageMsg: "success",
			TxResults: map[string]btcjson.SubmitPackageTxResult{
				"wtxid-1": {
					TxID:  chainhash.Hash{1},
					Error: &rejectReason,
				},
			},
		},
	})

	err := backend.SubmitPackage(
		t.Context(), []*wire.MsgTx{wire.NewMsgTx(3)}, wire.NewMsgTx(3),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "insufficient fee")
}

// TestSubmitPackageRejectedWithoutTxErrors verifies that a package-level
// rejection without any per-tx errors produces a clean error message (not
// "%!w(<nil>)") and still preserves the PackageMsg for diagnostics.
func TestSubmitPackageRejectedWithoutTxErrors(t *testing.T) {
	t.Parallel()

	backend := NewLNDBackend(
		&stubNotifier{}, &stubFeeEstimator{}, &stubBroadcaster{},
	)
	backend.SetPackageSubmitter(&stubPackageSubmitter{
		result: &btcjson.SubmitPackageResult{
			PackageMsg: "package-mempool-limits",
			TxResults:  map[string]btcjson.SubmitPackageTxResult{},
		},
	})

	err := backend.SubmitPackage(
		t.Context(), []*wire.MsgTx{wire.NewMsgTx(3)}, wire.NewMsgTx(3),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "package not accepted")
	require.Contains(t, err.Error(), "package-mempool-limits")
	require.NotContains(t, err.Error(), "%!w(<nil>)")
}

const (
	pollInterval = 50 * time.Millisecond
	testTimeout  = 5 * pollInterval
)
