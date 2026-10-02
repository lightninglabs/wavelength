package txconfirm

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/stretchr/testify/require"
)

// TestSpentInputRebroadcastKeepsConfirmation proves that a spent-input error
// cannot terminally fail a confirmed transaction before its notification is
// processed, even when the caller has not opted into broadcast retries.
func TestSpentInputRebroadcastKeepsConfirmation(t *testing.T) {
	for _, broadcastErr := range []error{
		fmt.Errorf("publish: transaction rejected: output already " +
			"spent"),
		fmt.Errorf("publish: %w", chain.ErrMissingInputs),
		fmt.Errorf("publish: %w", chain.ErrMissingInputsOrSpent),
		fmt.Errorf("sendrawtransaction: " +
			"bad-txns-inputs-missingorspent"),
	} {
		t.Run(broadcastErr.Error(), func(t *testing.T) {
			chainSource := newFakeChainSourceRef(100)
			tx := makeTestTx(false)
			chainSource.broadcastErr = broadcastErr
			chainSource.alreadyConfirmed[tx.TxHash()] =
				chainsource.ConfirmationEvent{
					Txid: tx.TxHash(), BlockHeight: 99,
					NumConfs: 1,
				}
			ref, _ := newTestActor(
				t, Config{
					ChainSource: chainSource,
				},
			)
			sub := actor.NewChannelTellOnlyRef[Notification](
				"mined", 4,
			)
			resp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
				Tx: tx, Subscriber: sub,
			})
			require.Equal(t, TxStateBroadcasting, resp.State)
			notification := mustAwaitNotification(t, sub)
			confirmed, ok := notification.(*TxConfirmed)
			require.True(
				t, ok,
				"must not report TxFailed before confirmation",
			)
			require.Equal(t, tx.TxHash(), confirmed.Txid)
			require.Equal(t, int32(99), confirmed.BlockHeight)
			require.Equal(t, 1, chainSource.broadcastCallCount())
		})
	}
}

// TestSpentInputWithoutConfirmationStaysUnproven covers the conflicting-spend
// control: keep the same candidate and watch, retry at the block interval, and
// never report acceptance merely because its inputs are spent.
func TestSpentInputWithoutConfirmationStaysUnproven(t *testing.T) {
	chainSource := newFakeChainSourceRef(100)
	chainSource.broadcastErr = fmt.Errorf("output already spent")
	ref, _ := newTestActor(t, Config{
		ChainSource: chainSource, FeeBumpIntervalBlocks: 2,
	})
	tx := makeTestTx(false)
	sub := actor.NewChannelTellOnlyRef[Notification]("unproven", 4)
	req := &EnsureConfirmedReq{Tx: tx, Subscriber: sub}
	require.Equal(
		t, TxStateBroadcasting, mustEnsure(t, ref.Ref(), req).State,
	)
	for _, step := range []struct {
		height int32
		calls  int
	}{{
		101,
		1,
	}, {
		102,
		2,
	}, {
		104,
		3,
	}} {
		chainSource.emitBlock(t, step.height)
		require.Equal(
			t, TxStateBroadcasting,
			mustEnsure(t, ref.Ref(), req).State,
		)
		require.Equal(t, step.calls, chainSource.broadcastCallCount())
	}
	mustHaveNoNotification(t, sub)
	require.Zero(t, chainSource.unregisterConfCount())
	chainSource.mu.Lock()
	for _, call := range chainSource.broadcastCalls {
		require.Equal(t, tx, call.Tx)
	}
	chainSource.mu.Unlock()

	// A delayed historical scan may still discover this exact transaction.
	chainSource.emitConfirmation(t, tx.TxHash(), 99)
	require.IsType(t, &TxConfirmed{}, mustAwaitNotification(t, sub))
}
