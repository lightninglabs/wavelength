package chainsource

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// closedFinalityBackend returns a stale buffered height on a closed first
// registration. A slow subscriber can retain this height while a later reorg
// overflows its queue; closure means its buffered tip is no longer current.
type closedFinalityBackend struct {
	*reconnectBlockBackend
	once   sync.Once
	height int32
}

// RegisterBlocks makes the first closed backlog deterministic before the actor
// can receive it, then exposes replacement registrations to the test.
func (b *closedFinalityBackend) RegisterBlocks(ctx context.Context) (
	*BlockRegistration, error) {

	first := false
	b.once.Do(func() { first = true })
	if first {
		epochs := make(chan *BlockEpoch, 1)
		epochs <- &BlockEpoch{Height: b.height + 5}
		close(epochs)

		return &BlockRegistration{
			Epochs: epochs, Cancel: func() {},
		}, nil
	}

	return b.reconnectBlockBackend.RegisterBlocks(ctx)
}

// TestConfActorReconcilesClosedFinalityBacklog proves a stale buffered height
// cannot finalize a confirmation after queue closure. The replacement arm
// checks the live tip before accepting new epochs.
func TestConfActorReconcilesClosedFinalityBacklog(t *testing.T) {
	t.Parallel()
	backend := &closedFinalityBackend{
		reconnectBlockBackend: newReconnectBlockBackend(), height: 100,
	}
	watch := NewConfActor(ConfActorConfig{
		Backend: backend, FinalityDepth: 6,
	})
	defer watch.Stop()
	positive := actor.NewChannelTellOnlyRef[ConfirmationEvent]("conf", 1)
	done := actor.NewChannelTellOnlyRef[ConfDoneEvent]("done", 1)
	txid := chainhash.Hash{1}
	result := watch.Receive(t.Context(), &RegisterConfRequest{
		CallerID: "closed-finality", Txid: &txid,
		PkScript: []byte{0x51}, TargetConfs: 1,
		NotifyActor: fn.Some[actor.TellOnlyRef[ConfirmationEvent]](
			positive,
		),
		NotifyDone: fn.Some[actor.TellOnlyRef[ConfDoneEvent]](done),
	})
	require.True(t, result.IsOk())
	hash := chainhash.Hash{2}
	backend.confChan <- &TxConfirmation{
		BlockHeight: 100, BlockHash: &hash, Tx: wire.NewMsgTx(2),
	}
	_, ok := positive.AwaitMessage(awaitTimeout)
	require.True(t, ok)
	_, ok = done.AwaitMessage(200 * time.Millisecond)
	require.False(t, ok, "closed backlog caused premature finality")

	replacement := backend.nextRegistration(t)
	replacement.send(105)
	_, ok = done.AwaitMessage(awaitTimeout)
	require.True(t, ok, "replacement stream did not restore finality")
}

// TestSpendActorReconcilesClosedFinalityBacklog is the spend-side proof that
// buffered heights from an overflowed stream cannot synthesize false finality.
func TestSpendActorReconcilesClosedFinalityBacklog(t *testing.T) {
	t.Parallel()
	backend := &closedFinalityBackend{
		reconnectBlockBackend: newReconnectBlockBackend(), height: 150,
	}
	backend.bestHeight = 150
	watch := NewSpendActor(SpendActorConfig{
		Backend: backend, FinalityDepth: 6,
	})
	defer watch.Stop()
	positive := actor.NewChannelTellOnlyRef[SpendEvent]("spend", 1)
	done := actor.NewChannelTellOnlyRef[SpendDoneEvent]("done", 1)
	outpoint := wire.OutPoint{Hash: chainhash.Hash{1}}
	result := watch.Receive(t.Context(), &RegisterSpendRequest{
		CallerID: "closed-finality", Outpoint: &outpoint,
		PkScript:    []byte{0x51},
		NotifyActor: fn.Some[actor.TellOnlyRef[SpendEvent]](positive),
		NotifyDone:  fn.Some[actor.TellOnlyRef[SpendDoneEvent]](done),
	})
	require.True(t, result.IsOk())
	tx := wire.NewMsgTx(2)
	txid := tx.TxHash()
	backend.spendChan <- &SpendDetail{
		SpentOutPoint: &outpoint, SpenderTxHash: &txid,
		SpendingTx: tx, SpendingHeight: 150,
	}
	_, ok := positive.AwaitMessage(awaitTimeout)
	require.True(t, ok)
	_, ok = done.AwaitMessage(200 * time.Millisecond)
	require.False(t, ok, "closed backlog caused premature finality")

	replacement := backend.nextRegistration(t)
	replacement.send(155)
	_, ok = done.AwaitMessage(awaitTimeout)
	require.True(t, ok, "replacement stream did not restore finality")
}
