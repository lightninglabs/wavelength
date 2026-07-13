package txconfirm

import (
	"testing"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/stretchr/testify/require"
)

// TestEnsureConfirmedReorgLifecycle drives the full reorg-aware lifecycle
// (Confirmed -> Reorged -> Confirmed -> Finalized) through the
// TxBroadcasterActor and asserts that:
//
//   - Each chainsource event reaches the subscriber as a matching public
//     notification, in order.
//   - The conf watch is held open across the reorg-out / re-confirm
//     bounce (no unregister fires before Done).
//   - Finalization releases the conf watch and evicts the tracked entry.
func TestEnsureConfirmedReorgLifecycle(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	ref, _ := newTestActor(t, Config{
		ChainSource: chain,
	})

	tx := makeTestTx(false)
	txid := tx.TxHash()
	sub := actor.NewChannelTellOnlyRef[Notification]("sub", 16)

	resp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: sub,
	})
	require.True(t, resp.Created)
	require.Equal(t, TxStateAwaitingConfirmation, resp.State)
	require.Equal(t, 1, chain.registerConfCount())

	// 1. First confirmation on the canonical chain.
	chain.emitConfirmation(t, txid, 101)
	first := mustAwaitNotification(t, sub)
	confirmed, ok := first.(*TxConfirmed)
	require.True(t, ok, "first event must be TxConfirmed")
	require.Equal(t, txid, confirmed.Txid)
	require.Equal(t, int32(101), confirmed.BlockHeight)

	// The conf watch must NOT have been released yet: the entry is
	// still in the reversible Confirmed state.
	require.Equal(t, 0, chain.unregisterConfCount())

	// 2. Reorg evicts that confirmation.
	chain.emitConfReorged(t, txid)
	second := mustAwaitNotification(t, sub)
	reorged, ok := second.(*TxReorged)
	require.True(t, ok, "second event must be TxReorged")
	require.Equal(t, txid, reorged.Txid)
	require.Equal(t, 0, chain.unregisterConfCount())

	// 3. Transaction re-confirms on the new tip.
	chain.emitConfirmation(t, txid, 102)
	third := mustAwaitNotification(t, sub)
	reConfirmed, ok := third.(*TxConfirmed)
	require.True(t, ok, "third event must be TxConfirmed")
	require.Equal(t, int32(102), reConfirmed.BlockHeight)
	require.Equal(t, 0, chain.unregisterConfCount())

	// 4. Finality. After TxFinalized the entry evicts.
	chain.emitConfDone(t, txid)
	fourth := mustAwaitNotification(t, sub)
	finalized, ok := fourth.(*TxFinalized)
	require.True(t, ok, "fourth event must be TxFinalized")
	require.Equal(t, txid, finalized.Txid)

	mustEventually(t, func() bool {
		return chain.unregisterConfCount() == 1
	})

	// Cancel for the now-evicted entry must observe an empty map: Removed
	// is false because there is nothing to remove.
	cancelResp := mustCancel(t, ref.Ref(), &CancelInterestReq{
		Txid:         txid,
		SubscriberID: sub.ID(),
	})
	require.False(t, cancelResp.Removed)
}

// TestEnsureConfirmedDoneDuringReorgGapDropped pins the documented
// edge-case semantics: if the chainsource backend fires Done while the
// tracked entry is in AwaitingConfirmation (post-reorg, pre-re-confirm),
// txconfirm drops the Done rather than advancing to Finalized. The
// realistic backends (chainntnfs, lndclient) do not fire Done during a
// reorg gap, but this test pins the guard so a future backend change
// cannot silently land the entry in Finalized off a non-Confirmed
// state, and so an accidental relaxation of the guard fails loudly.
func TestEnsureConfirmedDoneDuringReorgGapDropped(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	ref, _ := newTestActor(t, Config{
		ChainSource: chain,
	})

	tx := makeTestTx(false)
	txid := tx.TxHash()
	sub := actor.NewChannelTellOnlyRef[Notification]("sub", 16)

	mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: sub,
	})

	// Confirm, then reorg out — entry is now in AwaitingConfirmation.
	chain.emitConfirmation(t, txid, 101)
	first := mustAwaitNotification(t, sub)
	_, ok := first.(*TxConfirmed)
	require.True(t, ok, "first event must be TxConfirmed")

	chain.emitConfReorged(t, txid)
	second := mustAwaitNotification(t, sub)
	_, ok = second.(*TxReorged)
	require.True(t, ok, "second event must be TxReorged")

	// Fire Done out-of-band, before any re-confirmation. txconfirm
	// must NOT advance the entry to Finalized, must NOT deliver
	// TxFinalized to the subscriber, and must NOT unregister the
	// conf watch.
	chain.emitConfDone(t, txid)

	// Re-issuing EnsureConfirmedReq for the same (txid, params) is a
	// no-op attach that flushes the mailbox: by the time the response
	// returns, the queued confirmationDoneMsg has been processed (or
	// in this case, logged and dropped). The reported state must
	// remain AwaitingConfirmation.
	probe := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: sub,
	})
	require.False(t, probe.Created)
	require.Equal(
		t, TxStateAwaitingConfirmation, probe.State,
		"Done during reorg gap must not promote entry to Finalized",
	)

	// No TxFinalized notification should have been delivered.
	mustHaveNoNotification(t, sub)

	require.Equal(
		t, 0, chain.unregisterConfCount(),
		"dropped Done must not release the conf watch",
	)
}
