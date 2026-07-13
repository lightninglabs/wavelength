package btcwbackend

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

// reorgWaitTimeout is the per-step deadline used by the btcwbackend
// forwarder reorg tests. The forwarder is a tight goroutine, so the
// timeout exists only to make a hang surface as a fast failure on
// slow CI.
const reorgWaitTimeout = 2 * time.Second

// fakeNotifier is a minimal chainntnfs.ChainNotifier whose Register*
// methods hand back a caller-supplied event struct. The neutrino-side
// of the real chainntnfs implementation produces events with all four
// channels populated (Confirmed/NegativeConf/Done for conf,
// Spend/Reorg/Done for spend), so the stub lets us drive a synthetic
// upstream lifecycle through the chainbackend forwarder without
// standing up a real neutrino chain service.
type fakeNotifier struct {
	confEvent  *chainntnfs.ConfirmationEvent
	spendEvent *chainntnfs.SpendEvent
}

// RegisterConfirmationsNtfn returns the stub's pre-built confirmation
// event.
func (n *fakeNotifier) RegisterConfirmationsNtfn(_ *chainhash.Hash, _ []byte, _,
	_ uint32, _ ...chainntnfs.NotifierOption) (
	*chainntnfs.ConfirmationEvent, error) {

	return n.confEvent, nil
}

// RegisterSpendNtfn returns the stub's pre-built spend event.
func (n *fakeNotifier) RegisterSpendNtfn(_ *wire.OutPoint, _ []byte, _ uint32) (
	*chainntnfs.SpendEvent, error) {

	return n.spendEvent, nil
}

// RegisterBlockEpochNtfn returns an empty block-epoch event. The
// reorg lifecycle tests do not exercise block epochs, so the channels
// are intentionally never written to.
func (n *fakeNotifier) RegisterBlockEpochNtfn(_ *chainntnfs.BlockEpoch) (
	*chainntnfs.BlockEpochEvent, error) {

	return &chainntnfs.BlockEpochEvent{
		Epochs: make(chan *chainntnfs.BlockEpoch),
		Cancel: func() {},
	}, nil
}

// Start satisfies the ChainNotifier interface; the stub has no real
// lifecycle.
func (n *fakeNotifier) Start() error { return nil }

// Started satisfies the ChainNotifier interface; the stub always
// reports started.
func (n *fakeNotifier) Started() bool { return true }

// Stop satisfies the ChainNotifier interface; the stub has no real
// lifecycle.
func (n *fakeNotifier) Stop() error { return nil }

// TestRegisterConfForwardsReorgAndDone drives the full confirmation
// lifecycle through neutrino's chainntnfs notifier into the btcwbackend
// forwarder and asserts each event arrives on the matching chainsource
// registration channel. The lifecycle is:
//
//	Confirmed -> NegativeConf -> Confirmed -> Done
//
// and the test additionally verifies the forwarder exits after Done
// by observing that the chainsource channels close.
func TestRegisterConfForwardsReorgAndDone(t *testing.T) {
	t.Parallel()

	confChan := make(chan *chainntnfs.TxConfirmation, 2)
	negChan := make(chan int32, 1)
	doneChan := make(chan struct{}, 1)
	notifier := &fakeNotifier{
		confEvent: &chainntnfs.ConfirmationEvent{
			Confirmed:    confChan,
			NegativeConf: negChan,
			Done:         doneChan,
			Cancel:       func() {},
		},
	}
	backend := &ChainBackend{notifier: notifier}

	reg, err := backend.RegisterConf(
		t.Context(), &chainhash.Hash{0x42}, []byte{0x51}, 1, 100, false,
	)
	require.NoError(t, err)

	// 1. First confirmation crosses the forwarder.
	hash1 := chainhash.Hash{0xaa}
	confChan <- &chainntnfs.TxConfirmation{
		BlockHash:   &hash1,
		BlockHeight: 123,
		Tx:          wire.NewMsgTx(2),
	}

	conf1 := awaitConfForward(t, reg.Confirmed)
	require.Equal(t, uint32(123), conf1.BlockHeight)
	require.Equal(t, hash1, *conf1.BlockHash)

	// 2. Reorg ping is forwarded as a single struct{} on Reorged. The
	// depth value carried on NegativeConf is intentionally dropped at
	// this layer so neutrino and LND backends present the same wire
	// shape to chainsource consumers.
	negChan <- 1

	awaitSeq(t, reg.Reorged, "Reorged forward")

	// 3. Transaction re-confirms in a different block on the new tip.
	hash2 := chainhash.Hash{0xbb}
	confChan <- &chainntnfs.TxConfirmation{
		BlockHash:   &hash2,
		BlockHeight: 124,
		Tx:          wire.NewMsgTx(2),
	}

	conf2 := awaitConfForward(t, reg.Confirmed)
	require.Equal(t, uint32(124), conf2.BlockHeight)
	require.Equal(t, hash2, *conf2.BlockHash)

	// 4. Done signal is forwarded; the forwarder then exits and the
	// chainsource channels close.
	doneChan <- struct{}{}

	awaitStruct(t, reg.Done, "Done forward")

	// All three forwarded channels must close once the forwarder
	// exits.
	requireConfClosedSoon(t, reg.Confirmed)
	requireSeqClosedSoon(t, reg.Reorged)
	requireStructClosedSoon(t, reg.Done)
}

// TestRegisterSpendForwardsReorgAndDone is the spend-side equivalent of
// the confirmation lifecycle test above.
func TestRegisterSpendForwardsReorgAndDone(t *testing.T) {
	t.Parallel()

	spendChan := make(chan *chainntnfs.SpendDetail, 2)
	reorgChan := make(chan struct{}, 1)
	doneChan := make(chan struct{}, 1)
	notifier := &fakeNotifier{
		spendEvent: &chainntnfs.SpendEvent{
			Spend:  spendChan,
			Reorg:  reorgChan,
			Done:   doneChan,
			Cancel: func() {},
		},
	}
	backend := &ChainBackend{notifier: notifier}

	outpoint := &wire.OutPoint{Index: 1}
	reg, err := backend.RegisterSpend(
		t.Context(), outpoint, []byte{0x51}, 100,
	)
	require.NoError(t, err)

	// 1. First spend.
	hash1 := chainhash.Hash{0x10}
	spendChan <- &chainntnfs.SpendDetail{
		SpentOutPoint:  outpoint,
		SpenderTxHash:  &hash1,
		SpendingTx:     wire.NewMsgTx(2),
		SpendingHeight: 150,
	}

	spend1 := awaitSpendForward(t, reg.Spend)
	require.Equal(t, int32(150), spend1.SpendingHeight)
	require.Equal(t, hash1, *spend1.SpenderTxHash)

	// 2. Reorg evicts the spend.
	reorgChan <- struct{}{}

	awaitSeq(t, reg.Reorged, "spend Reorged forward")

	// 3. A different spender wins the new chain.
	hash2 := chainhash.Hash{0x20}
	spendChan <- &chainntnfs.SpendDetail{
		SpentOutPoint:  outpoint,
		SpenderTxHash:  &hash2,
		SpendingTx:     wire.NewMsgTx(2),
		SpendingHeight: 151,
	}

	spend2 := awaitSpendForward(t, reg.Spend)
	require.Equal(t, int32(151), spend2.SpendingHeight)
	require.Equal(t, hash2, *spend2.SpenderTxHash)

	// 4. Done.
	doneChan <- struct{}{}

	awaitStruct(t, reg.Done, "spend Done forward")

	requireSpendClosedSoon(t, reg.Spend)
	requireSeqClosedSoon(t, reg.Reorged)
	requireStructClosedSoon(t, reg.Done)
}

// awaitConfForward reads a single confirmation off the forwarded
// channel with a deadline, failing the test on timeout or unexpected
// close.
func awaitConfForward(t *testing.T,
	ch <-chan *chainsource.TxConfirmation) *chainsource.TxConfirmation {

	t.Helper()

	select {
	case conf, ok := <-ch:
		if !ok {
			t.Fatal("conf channel closed before delivery")
		}

		return conf

	case <-time.After(reorgWaitTimeout):
		t.Fatal("timeout waiting for conf forward")

		return nil
	}
}

// awaitSpendForward reads a single spend off the forwarded channel
// with a deadline, failing the test on timeout or unexpected close.
func awaitSpendForward(t *testing.T,
	ch <-chan *chainsource.SpendDetail) *chainsource.SpendDetail {

	t.Helper()

	select {
	case spend, ok := <-ch:
		if !ok {
			t.Fatal("spend channel closed before delivery")
		}

		return spend

	case <-time.After(reorgWaitTimeout):
		t.Fatal("timeout waiting for spend forward")

		return nil
	}
}

// awaitStruct reads a single struct{} off the forwarded channel with a
// deadline. Used for the Reorged and Done channels.
func awaitStruct(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()

	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatalf("%s channel closed before delivery", label)
		}

	case <-time.After(reorgWaitTimeout):
		t.Fatalf("timeout waiting for %s", label)
	}
}

// requireConfClosedSoon asserts the confirmation channel closes within
// the reorg wait timeout. Used to verify the forwarder's defer-close
// chain runs after Done is delivered.
func requireConfClosedSoon(t *testing.T,
	ch <-chan *chainsource.TxConfirmation) {

	t.Helper()

	require.Eventually(t, func() bool {
		select {
		case _, ok := <-ch:
			return !ok

		default:
			return false
		}
	}, reorgWaitTimeout, 10*time.Millisecond,
		"conf channel did not close after Done")
}

// requireSpendClosedSoon asserts the spend channel closes within the
// reorg wait timeout.
func requireSpendClosedSoon(t *testing.T, ch <-chan *chainsource.SpendDetail) {
	t.Helper()

	require.Eventually(t, func() bool {
		select {
		case _, ok := <-ch:
			return !ok

		default:
			return false
		}
	}, reorgWaitTimeout, 10*time.Millisecond,
		"spend channel did not close after Done")
}

// requireStructClosedSoon asserts that a struct{} signal channel
// closes within the reorg wait timeout.
func requireStructClosedSoon(t *testing.T, ch <-chan struct{}) {
	t.Helper()

	require.Eventually(t, func() bool {
		select {
		case _, ok := <-ch:
			return !ok

		default:
			return false
		}
	}, reorgWaitTimeout, 10*time.Millisecond,
		"struct channel did not close after Done")
}

// awaitSeq reads a single sequence value off the forwarded Reorged
// channel with a deadline. chainsource retyped Reorged from struct{}
// to the ordering sequence; this backend always sends 0.
func awaitSeq(t *testing.T, ch <-chan uint64, label string) {
	t.Helper()

	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatalf("%s channel closed before delivery", label)
		}

	case <-time.After(reorgWaitTimeout):
		t.Fatalf("timeout waiting for %s", label)
	}
}

// requireSeqClosedSoon asserts that a sequence-carrying signal channel
// closes within the reorg wait timeout.
func requireSeqClosedSoon(t *testing.T, ch <-chan uint64) {
	t.Helper()

	require.Eventually(t, func() bool {
		select {
		case _, ok := <-ch:
			return !ok

		default:
			return false
		}
	}, reorgWaitTimeout, 10*time.Millisecond,
		"seq channel did not close after Done")
}
