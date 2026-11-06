package actordelivery

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/timeout"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// roundRuntimeQuery returns a fixed response for a process-local dependency.
type roundRuntimeQuery[M actor.Message, R any] struct {
	actor.ActorRef[M, R]
	response R
}

// Ask supplies the configured dependency response without spawning a worker.
func (r *roundRuntimeQuery[M, R]) Ask(_ context.Context, _ M) actor.Future[R] {
	promise := actor.NewPromise[R]()
	promise.Complete(fn.Ok(r.response))

	return promise.Future()
}

// TestRoundRuntimeRestart restores a pre-admission round through the actual
// durable mailbox and SQL checkpoint store. No legacy active-round row exists
// for this state, so a domain recovery scan cannot make this test pass.
func TestRoundRuntimeRestart(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		name := "committed"
		if rollback {
			name = "rollback"
		}
		t.Run(name, func(t *testing.T) {
			testRoundRuntimeRestart(t, rollback)
		})
	}
}

// roundCheckpointFault rolls back a final checkpoint after the turn has
// attempted its outbox writes. Stage checkpoints still commit normally.
type roundCheckpointFault struct {
	actor.TxAwareDeliveryStore
	fail     atomic.Bool
	rejected chan struct{}
}

// roundFaultTx preserves the real SQL transaction while injecting its failure.
type roundFaultTx struct {
	actor.DeliveryStore
	owner *roundCheckpointFault
}

// SaveCheckpoint fails exactly one final commit, after prior transaction
// writes.
func (s *roundFaultTx) SaveCheckpoint(ctx context.Context,
	p actor.CheckpointParams) error {

	if p.StateType == "client-round" &&
		s.owner.fail.CompareAndSwap(true, false) {

		close(s.owner.rejected)

		return errors.New("injected round checkpoint failure")
	}

	return s.DeliveryStore.SaveCheckpoint(ctx, p)
}

// ExecTx wraps the transaction-scoped store, so failure exercises SQL rollback.
func (s *roundCheckpointFault) ExecTx(ctx context.Context, readOnly bool,
	f actor.TxFunc) error {

	return s.TxAwareDeliveryStore.ExecTx(
		ctx, readOnly,
		func(txCtx context.Context, tx actor.DeliveryStore) error {
			return f(
				txCtx, &roundFaultTx{
					DeliveryStore: tx,
					owner:         s,
				},
			)
		},
	)
}

// testRoundRuntimeRestart verifies committed and rolled-back input recovery
// against one shared SQL database and two separately owned actor lifetimes.
func testRoundRuntimeRestart(t *testing.T, rollback bool) {
	t.Helper()
	testDB := db.NewTestDB(t)
	clk := clock.NewDefaultClock()
	delivery, err := NewTxAwareDeliveryStoreFromDB(
		testDB.DB, testDB.Backend(), clk, btclog.Disabled,
	)
	require.NoError(t, err)
	store := &roundCheckpointFault{
		TxAwareDeliveryStore: delivery,
		rejected:             make(chan struct{}),
	}
	domain := db.NewStore(
		testDB.DB, testDB.Queries, testDB.Backend(), btclog.Disabled,
	)
	serverRef := actor.NewChannelTellOnlyRef[serverconn.ServerConnMsg](
		"server", 1,
	)
	timerRef := actor.NewChannelTellOnlyRef[timeout.Msg]("timer", 1)
	walletRef := &roundRuntimeQuery[wallet.WalletMsg, wallet.WalletResp]{
		response: &wallet.RegisterConfirmationNotifierResponse{
			Success: true,
		},
	}
	chainRef := &roundRuntimeQuery[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]{response: &chainsource.BestHeightResponse{
		Height: 100,
	}}
	cfg := &round.RoundClientConfig{
		Name: "round-client-mailbox", Logger: btclog.Disabled,
		RoundStore: domain.NewRoundStore(
			&chaincfg.MainNetParams, clk,
		),
		VTXOStore: domain.NewRoundStore(
			&chaincfg.MainNetParams, clk,
		),
		OperatorTerms: &types.OperatorTerms{
			MinConfirmations: 1,
		},
		ChainParams:  &chaincfg.MainNetParams,
		WalletActor:  walletRef,
		ChainSource:  chainRef,
		ServerConn:   serverRef,
		TimeoutActor: timerRef,
	}
	start := func() *round.DurableRoundClientActor {
		runtime, err := round.NewDurableRoundClientActor(cfg, store)
		require.NoError(t, err)
		system := actor.NewActorSystem()
		require.NoError(
			t,
			actor.RegisterWithReceptionist(
				system.Receptionist(), round.NewServiceKey(),
				runtime.Ref(),
			),
		)
		require.NoError(
			t,
			actor.RegisterWithReceptionist(
				system.Receptionist(),
				actor.NewServiceKey[actor.Message, any](
					cfg.Name,
				),
				runtime.OutboxRef(),
			),
		)
		t.Cleanup(func() {
			require.NoError(
				t,
				system.Shutdown(
					context.Background(),
				),
			)
		})
		startupCtx, cancelStartup := context.WithCancel(t.Context())
		require.NoError(t, runtime.Start(startupCtx))
		cancelStartup()
		queryCtx, cancelQuery := context.WithTimeout(
			t.Context(), 5*time.Second,
		)
		defer cancelQuery()
		_, err = round.NewServiceKey().Ref(system).Ask(
			queryCtx, &round.GetClientStateRequest{},
		).Await(queryCtx).Unpack()
		require.NoError(t, err)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(
				context.Background(), 5*time.Second,
			)
			defer cancel()
			require.NoError(t, runtime.StopAndWait(ctx))
		})

		return runtime
	}
	first := start()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	baseline, err := store.LoadCheckpoint(ctx, cfg.Name)
	require.NoError(t, err)
	_, err = first.Ref().Ask(ctx, &round.RegisterIntentRequest{
		Package: &round.IntentPackage{Intents: round.Intents{
			Forfeits: []types.ForfeitRequest{{
				VTXOOutpoint: &wire.OutPoint{Index: 7},
			}},
			VTXOs: []types.VTXORequest{{
				Amount: 321, FixedAmount: true,
			}},
		}},
	}).Await(ctx).Unpack()
	require.NoError(t, err)
	if rollback {
		baseline, err = store.LoadCheckpoint(ctx, cfg.Name)
		require.NoError(t, err)
		store.fail.Store(true)
		// Remote mailbox ingress admits Tell deliveries. A failed Tell
		// must remain queued, even though the final transaction already
		// tried effects.
		require.NoError(
			t,
			first.Ref().Tell(ctx, &round.CancelRoundRequest{}),
		)
		select {
		case <-store.rejected:
		case <-ctx.Done():
			t.Fatal("cancel did not reach injected commit failure")
		}
	}
	// Join before inspecting persisted rows: the owner must have finished
	// nacking the failed input, rather than racing test teardown.
	require.NoError(t, first.StopAndWait(ctx))
	checkpoint, err := store.LoadCheckpoint(ctx, cfg.Name)
	require.NoError(t, err)
	require.NotNil(t, checkpoint)
	if rollback {
		require.Equal(t, baseline.Version, checkpoint.Version)
		require.Equal(t, "client-round-in-flight", checkpoint.StateType)
		// The row may be in retry backoff. ListPendingMailboxIDs must
		// still expose it for restart recovery.
		lister, ok := delivery.(actor.PendingMailboxLister)
		require.True(t, ok)
		ids, err := lister.ListPendingMailboxIDs(ctx, cfg.Name)
		require.NoError(t, err)
		require.Contains(t, ids, cfg.Name)
	} else {
		require.Equal(t, "client-round", checkpoint.StateType)
		require.Greater(t, checkpoint.Version, baseline.Version)
	}
	// No uncommitted timer may escape the failed final transaction.
	effects, err := store.ClaimOutboxBatch(ctx, actor.OutboxClaimParams{
		Limit: 10, ClaimToken: "inspect",
		ClaimDuration: time.Nanosecond,
	})
	require.NoError(t, err)
	require.Empty(t, effects)

	second := start()
	var state *round.GetClientStateResponse
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		after, err := second.Ref().Ask(
			ctx, &round.GetClientStateRequest{},
		).Await(ctx).Unpack()
		require.NoError(collect, err)
		var ok bool
		state, ok = after.(*round.GetClientStateResponse)
		require.True(collect, ok)
		if rollback {
			require.Empty(collect, state.States)
		} else {
			require.Len(collect, state.States, 1)
		}
	}, 5*time.Second, 20*time.Millisecond)
	for _, info := range state.States {
		pending, ok := info.State.(*round.PendingRoundAssembly)
		require.True(t, ok)
		require.EqualValues(t, 321, pending.VTXOs[0].Amount)
	}
	if rollback {
		effects, err = store.ClaimOutboxBatch(
			ctx, actor.OutboxClaimParams{
				Limit:         10,
				ClaimToken:    "after-recovery",
				ClaimDuration: time.Minute,
			},
		)
		require.NoError(t, err)
		require.Len(
			t, effects, 1,
			"only the committed reservation release may escape",
		)
	}
	require.Empty(t, serverRef.Messages())
	require.Empty(t, timerRef.Messages())
}
