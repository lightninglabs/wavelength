package unroll

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/actordelivery"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/unrollplan"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// retrySweepChain injects an ambiguous submission failure at the chain boundary
// while retaining real unroll and txconfirm actors on either side of restart.
type retrySweepChain struct {
	*fakeChainSourceRef
	retryMu  sync.Mutex
	failure  error
	attempts chan *wire.MsgTx
	blocks   map[string]actor.TellOnlyRef[chainsource.BlockEpoch]
}

// Ask captures exact broadcast bytes and keeps block subscriptions by owner.
func (c *retrySweepChain) Ask(ctx context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	c.retryMu.Lock()
	defer c.retryMu.Unlock()
	if req, ok := msg.(*chainsource.SubscribeBlocksRequest); ok {
		c.blocks[req.CallerID] = req.NotifyActor.UnwrapOr(nil)
	}
	if req, ok := msg.(*chainsource.BroadcastTxRequest); ok {
		c.attempts <- req.Tx.Copy()
		promise := actor.NewPromise[chainsource.ChainSourceResp]()
		if c.failure != nil {
			promise.Complete(
				fn.Err[chainsource.ChainSourceResp](c.failure),
			)
		} else {
			promise.Complete(
				fn.Ok[chainsource.ChainSourceResp](
					&chainsource.BroadcastTxResponse{},
				),
			)
		}

		return promise.Future()
	}

	return c.fakeChainSourceRef.Ask(ctx, msg)
}

// advance delivers a new block after changing the injected broadcast outcome.
func (c *retrySweepChain) advance(t *testing.T, height int32, failure error) {
	t.Helper()
	c.retryMu.Lock()
	c.failure = failure
	refs := make(
		[]actor.TellOnlyRef[chainsource.BlockEpoch], 0, len(c.blocks),
	)
	for _, ref := range c.blocks {
		refs = append(refs, ref)
	}
	c.retryMu.Unlock()
	for _, ref := range refs {
		require.NoError(
			t,
			ref.Tell(
				t.Context(), chainsource.BlockEpoch{
					Height: height,
				},
			),
		)
	}
}

// nextAttempt waits for a real broadcaster submission rather than polling time.
func (c *retrySweepChain) nextAttempt(t *testing.T) *wire.MsgTx {
	t.Helper()
	select {
	case tx := <-c.attempts:
		return tx

	case <-time.After(5 * time.Second):
		t.Fatal("sweep was not submitted")

		return nil
	}
}

// TestSweepTransientFailureRestartRecovery proves the final anchorless sweep
// survives a transient submission error and full actor restart. Its serialized
// SQLite checkpoint restores the same signed transaction, retry contract, and
// unconsumed unroll budget; confirmation then completes the exit.
func TestSweepTransientFailureRestartRecovery(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	sqlDB := db.NewTestDB(t)
	delivery, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		sqlDB.DB, sqlDB.Backend(), clock.NewDefaultClock(),
		btclog.Disabled,
	)
	require.NoError(t, err)
	const actorID = "retry-sweep-restart"
	raw, err := encodeCheckpoint(&actorCheckpoint{
		Version: checkpointVersion, Height: 110, Started: true,
		Trigger: TriggerRestart,
		State: unrollplan.State{
			ConfirmedTxids: []chainhash.Hash{
				proof.RootTxids()[0],
				proof.TargetOutpoint().Hash,
			},
			TargetConfirmHeight: fn.Some[int32](108),
		},
	})
	require.NoError(t, err)
	require.NoError(
		t,
		delivery.SaveCheckpoint(
			t.Context(), actor.CheckpointParams{
				ActorID:   actorID,
				StateType: checkpointStateType,
				StateData: raw,
				Version:   checkpointVersion,
			},
		),
	)
	chain := &retrySweepChain{
		fakeChainSourceRef: &fakeChainSourceRef{
			bestHeight: 110,
		},
		failure: fmt.Errorf("backend unavailable after " +
			"submission"),
		attempts: make(chan *wire.MsgTx, 16),
		blocks: make(
			map[string]actor.TellOnlyRef[chainsource.BlockEpoch],
		),
	}
	wallet := &fakeSweepWallet{}
	var original *wire.MsgTx
	for generation := 0; generation < 2; generation++ {
		txBehavior := txconfirm.NewTxBroadcasterActor(txconfirm.Config{
			ChainSource: chain, FeeBumpIntervalBlocks: 2,
		})
		txActor := actor.NewActor(actor.ActorConfig[
			txconfirm.Msg,
			txconfirm.Resp,
		]{
			ID:          "retry-txconfirm",
			Behavior:    txBehavior,
			MailboxSize: 64,
		})
		txBehavior.SetSelfRef(txActor.TellRef())
		txActor.Start()
		t.Cleanup(txActor.Stop)
		exit, err := NewVTXOUnrollActor(
			Config{
				ActorID:        actorID,
				TargetOutpoint: proof.TargetOutpoint(),
				DeliveryStore:  delivery,
				ProofAssembler: &mockProofAssembler{
					proof: proof,
				},
				VTXOStore:    &mockVTXOStore{desc: desc},
				TxConfirmRef: txActor.Ref(),
				ChainSource:  chain,
				Wallet:       wallet,
			},
		)
		require.NoError(t, err)
		t.Cleanup(exit.Stop)
		mustAsk(t, exit.Ref(), &ResumeUnrollRequest{Height: 110})
		attempt := chain.nextAttempt(t)
		if original == nil {
			original = attempt
		} else {
			requireSameSweep(t, original, attempt)
		}
		response := mustAsk(t, exit.Ref(), &GetStateRequest{})
		state, ok := response.(*GetStateResp)
		require.True(t, ok)
		require.Equal(t, PhaseSweepConfirmation, state.Phase)
		checkpoint, err := delivery.LoadCheckpoint(t.Context(), actorID)
		require.NoError(t, err)
		saved, err := decodeCheckpoint(checkpoint.StateData)
		require.NoError(t, err)
		require.Zero(t, saved.SweepAttempts)
		requireSameSweep(t, original, saved.SweepTx)
		require.Equal(t, int64(1), wallet.pkScriptRequestCount())
		if generation == 1 {
			chain.advance(t, 112, nil)
			requireSameSweep(t, original, chain.nextAttempt(t))
			chain.emitConfirmed(t, original.TxHash(), 113)
			require.Eventually(t, func() bool {
				state, ok := mustAsk(
					t, exit.Ref(), &GetStateRequest{},
				).(*GetStateResp)

				return ok && state.Phase == PhaseCompleted
			}, 5*time.Second, 10*time.Millisecond)
		}
		exit.Stop()
		txActor.Stop()
	}
}

// requireSameSweep compares wire bytes, including witnesses, across checkpoint
// decoding, where empty script slices may differ from nil slices in memory.
func requireSameSweep(t *testing.T, want, got *wire.MsgTx) {
	t.Helper()
	var wantBytes, gotBytes bytes.Buffer
	require.NoError(t, want.Serialize(&wantBytes))
	require.NoError(t, got.Serialize(&gotBytes))
	require.Equal(t, wantBytes.Bytes(), gotBytes.Bytes())
	require.Equal(t, want.TxHash(), got.TxHash())
}
