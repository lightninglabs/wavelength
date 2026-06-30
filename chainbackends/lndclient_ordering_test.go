package chainbackends

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

// orderingWaitTimeout bounds each step of the ordered-forwarder tests so a
// hang (a forwarder that never delivers) surfaces as a fast failure.
const orderingWaitTimeout = 2 * time.Second

// TestForwardOrderedReorgPrioritizesReorg pins the cross-channel ordering
// guarantee of the conf-path forwarder: when a reorg ping and a confirmation
// are BOTH already pending (the exact race the two-goroutine split created),
// the forwarder must deliver the NegativeConf (reorg) before the Confirmed,
// preserving lndclient's reorg-before-reconfirmation order so the downstream
// ConfActor cannot reset confirmHeight after a re-confirmation and strand the
// watch.
func TestForwardOrderedReorgPrioritizesReorg(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Both sources ready up front: lndclient wrote the reorg before the
	// replacement confirmation, and both are buffered by the time the
	// forwarder runs. The reorg carries its depth (3).
	reorgDepth := make(chan int32, 1)
	confChan := make(chan *chainntnfs.TxConfirmation, 1)
	reorgDepth <- 3
	confChan <- &chainntnfs.TxConfirmation{BlockHeight: 100}

	// The downstream channels are unbuffered so the forwarder cannot run
	// ahead: it must block handing off the reorg before it can forward the
	// confirmation, exactly as the single in-order consumer it feeds in
	// production behaves. Buffering these would let both hand-offs complete
	// before the assertion runs and make the ordering check race.
	outConfirmed := make(chan *chainntnfs.TxConfirmation)
	outNegConf := make(chan int32)

	go forwardOrderedReorg(
		ctx, reorgDepth, confChan, outConfirmed, outNegConf,
	)

	// The reorg must arrive first, carrying the forwarded depth.
	select {
	case depth := <-outNegConf:
		require.EqualValues(t, 3, depth)

	case <-outConfirmed:
		t.Fatal("confirmation delivered before reorg")

	case <-time.After(orderingWaitTimeout):
		t.Fatal("timeout waiting for the reorg")
	}

	// Then the confirmation.
	select {
	case c := <-outConfirmed:
		require.EqualValues(t, 100, c.BlockHeight)

	case <-time.After(orderingWaitTimeout):
		t.Fatal("timeout waiting for the confirmation")
	}
}

// TestForwardOrderedSpendReorgPrioritizesReorg is the spend-path analogue:
// a pending reorg must be delivered on Reorg before a pending re-Spend.
func TestForwardOrderedSpendReorgPrioritizesReorg(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	reorgPing := make(chan struct{}, 1)
	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	reorgPing <- struct{}{}
	spendChan <- &chainntnfs.SpendDetail{SpendingHeight: 100}

	// Unbuffered downstream channels so the forwarder cannot run ahead of
	// the in-order consumer; see the conf-path test for the rationale.
	outSpend := make(chan *chainntnfs.SpendDetail)
	outReorg := make(chan struct{})

	go forwardOrderedSpendReorg(
		ctx, reorgPing, spendChan, outSpend, outReorg,
	)

	select {
	case <-outReorg:
	case <-outSpend:
		t.Fatal("spend delivered before reorg")

	case <-time.After(orderingWaitTimeout):
		t.Fatal("timeout waiting for the reorg")
	}

	select {
	case sp := <-outSpend:
		require.EqualValues(t, 100, sp.SpendingHeight)

	case <-time.After(orderingWaitTimeout):
		t.Fatal("timeout waiting for the spend")
	}
}

// TestForwardOrderedReorgForwardsConfirmationAlone verifies the common path:
// with no reorg pending, a confirmation is forwarded unchanged.
func TestForwardOrderedReorgForwardsConfirmationAlone(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	reorgDepth := make(chan int32, 1)
	confChan := make(chan *chainntnfs.TxConfirmation, 1)
	confChan <- &chainntnfs.TxConfirmation{BlockHeight: 42}

	outConfirmed := make(chan *chainntnfs.TxConfirmation, 1)
	outNegConf := make(chan int32, 1)

	go forwardOrderedReorg(
		ctx, reorgDepth, confChan, outConfirmed, outNegConf,
	)

	select {
	case c := <-outConfirmed:
		require.EqualValues(t, 42, c.BlockHeight)

	case <-outNegConf:
		t.Fatal("unexpected reorg with none pending")

	case <-time.After(orderingWaitTimeout):
		t.Fatal("timeout waiting for the confirmation")
	}
}
