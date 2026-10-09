package waved

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/chainbackends"
	"github.com/lightninglabs/wavelength/internal/expiryfixture"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// repairScanHintCache deliberately retains no hints: the test must recover
// through the notifier's retained scan state, not an improved starting hint.
type repairScanHintCache struct{}

// QueryConfirmHint makes every subscription start with its caller's hint.
func (repairScanHintCache) QueryConfirmHint(chainntnfs.ConfRequest) (uint32,
	error) {

	return 0, chainntnfs.ErrConfirmHintNotFound
}

// CommitConfirmHint discards hints so they cannot hide a restarted scan.
func (repairScanHintCache) CommitConfirmHint(uint32,
	...chainntnfs.ConfRequest) error {

	return nil
}

// PurgeConfirmHint satisfies the cache contract without retained hints.
func (repairScanHintCache) PurgeConfirmHint(...chainntnfs.ConfRequest) error {
	return nil
}

// repairScanNotifier delegates subscriptions and cancellation to the pinned
// LND notifier. Only historical scan completion is supplied by the test.
type repairScanNotifier struct {
	chainntnfs.ChainNotifier
	core       *chainntnfs.TxNotifier
	dispatches []*chainntnfs.HistoricalConfDispatch
	cancelled  chan struct{}
}

// RegisterConfirmationsNtfn records requested scans without completing them.
// Cancellation uses the real notifier's CancelConf through the returned event.
func (n *repairScanNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	script []byte, confs, hint uint32, opts ...chainntnfs.NotifierOption) (
	*chainntnfs.ConfirmationEvent, error) {

	reg, err := n.core.RegisterConf(txid, script, confs, hint, opts...)
	if err != nil {
		return nil, err
	}
	if reg.HistoricalDispatch != nil {
		n.dispatches = append(n.dispatches, reg.HistoricalDispatch)
	}
	var once sync.Once
	cancel := reg.Event.Cancel
	reg.Event.Cancel = func() {
		once.Do(func() {
			cancel()
			n.cancelled <- struct{}{}
		})
	}

	return reg.Event, nil
}

// TestRepairLegacyCommitmentHeightsRetainsHistoricalScan proves that bounded
// subscriptions do not restart LND's historical scan. Two repair attempts time
// out and cancel before the scan completes; a third consumes its cached result
// through the real backend forwarder and atomically repairs the descriptor.
// This exercises notifier state, not a live block download or mined reorg.
func TestRepairLegacyCommitmentHeightsRetainsHistoricalScan(t *testing.T) {
	t.Parallel()
	s, rpc, descs := newCommitmentRepairServer(
		t, 1,
		clock.NewTestClock(
			time.Now(),
		),
	)
	desc := descs[0]
	before, err := s.vtxoStore.GetVTXO(t.Context(), desc.Outpoint)
	require.NoError(t, err)
	rpc.byScript[string(desc.PkScript)].AncestryPaths[0].
		CommitmentHeight = 326
	_, commitment := expiryfixture.Round(
		t, 10000, desc.PkScript, 50, 326, 1,
	)
	require.Equal(t, desc.Ancestry[0].CommitmentTxID, commitment.TxHash())

	// Put the confirmation far behind the tip. The scan remains pending
	// across caller timeouts, regardless of how long the backend takes.
	const tip = 500_000
	core := chainntnfs.NewTxNotifier(tip, 100, repairScanHintCache{}, nil)
	t.Cleanup(core.TearDown)
	notifier := &repairScanNotifier{
		core:      core,
		cancelled: make(chan struct{}, 3),
	}
	backend := chainbackends.NewLNDBackend(notifier, nil, nil)
	s.chainBackend = &laterHeightBackend{
		heightOnlyChainBackend: heightOnlyChainBackend{
			height: tip,
		},
		register: backend.RegisterConf,
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range 2 {
		result, err := s.repairLegacyCommitmentHeightsWithTimeout(
			ctx, fastCommitmentRepairPage, 20*time.Millisecond,
		)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.NoError(t, ctx.Err())
		require.Zero(t, result.completed)
		select {
		case <-notifier.cancelled:
		case <-ctx.Done():
			t.Fatal("confirmation subscription was not cancelled")
		}
		require.Len(t, notifier.dispatches, 1, "scan restarted")
		unchanged, err := s.vtxoStore.GetVTXO(ctx, desc.Outpoint)
		require.NoError(t, err)
		require.Equal(t, before, unchanged)
	}

	// Complete the backend-owned scan with no repair subscriber left.
	// The next subscription must receive that evidence without rescanning.
	dispatch := notifier.dispatches[0]
	require.Equal(t, uint32(desc.CreatedHeight), dispatch.StartHeight)
	require.Equal(t, uint32(tip), dispatch.EndHeight)
	require.NoError(
		t,
		core.UpdateConfDetails(
			dispatch.ConfRequest, &chainntnfs.TxConfirmation{
				Tx:          commitment,
				BlockHeight: 326,
			},
		),
	)
	result, err := s.repairLegacyCommitmentHeights(
		ctx, fastCommitmentRepairPage,
	)
	require.NoError(t, err)
	require.Equal(t, 1, result.completed)
	require.Len(t, notifier.dispatches, 1)
	select {
	case <-notifier.cancelled:
	case <-ctx.Done():
		t.Fatal(
			"successful confirmation subscription was not " +
				"cancelled",
		)
	}
	after, err := s.vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	before.Ancestry[0].CommitmentHeight = 326
	require.Equal(t, before, after)
}
