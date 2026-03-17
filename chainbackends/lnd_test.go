package chainbackends

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

type stubNotifier struct {
	confEvent  *chainntnfs.ConfirmationEvent
	spendEvent *chainntnfs.SpendEvent
}

func (n *stubNotifier) RegisterConfirmationsNtfn(
	_ *chainhash.Hash, _ []byte, _, _ uint32,
	_ ...chainntnfs.NotifierOption,
) (*chainntnfs.ConfirmationEvent, error) {

	return n.confEvent, nil
}

func (n *stubNotifier) RegisterSpendNtfn(
	_ *wire.OutPoint, _ []byte, _ uint32,
) (*chainntnfs.SpendEvent, error) {

	return n.spendEvent, nil
}

func (n *stubNotifier) RegisterBlockEpochNtfn(
	_ *chainntnfs.BlockEpoch,
) (*chainntnfs.BlockEpochEvent, error) {

	return &chainntnfs.BlockEpochEvent{
		Epochs: make(chan *chainntnfs.BlockEpoch),
		Cancel: func() {},
	}, nil
}

func (n *stubNotifier) Start() error { return nil }

func (n *stubNotifier) Started() bool { return true }

func (n *stubNotifier) Stop() error { return nil }

type stubFeeEstimator struct{}

func (s *stubFeeEstimator) EstimateFeePerKW(
	_ uint32,
) (chainfee.SatPerKWeight, error) {

	return 0, nil
}

func (s *stubFeeEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	return 0
}

func (s *stubFeeEstimator) Start() error { return nil }

func (s *stubFeeEstimator) Stop() error { return nil }

type stubBroadcaster struct{}

func (s *stubBroadcaster) PublishTransaction(
	_ context.Context, _ *wire.MsgTx, _ string,
) error {

	return nil
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

const (
	pollInterval = 50 * time.Millisecond
	testTimeout  = 5 * pollInterval
)
