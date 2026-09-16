package actordelivery

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
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
	testDB := db.NewTestDB(t)
	clk := clock.NewDefaultClock()
	store, err := NewTxAwareDeliveryStoreFromDB(
		testDB.DB, testDB.Backend(), clk, btclog.Disabled,
	)
	require.NoError(t, err)
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
	_, err = first.Ref().Ask(ctx, &round.RegisterIntentRequest{
		Package: &round.IntentPackage{Intents: round.Intents{
			VTXOs: []types.VTXORequest{{
				Amount: 321, FixedAmount: true,
			}},
		}},
	}).Await(ctx).Unpack()
	require.NoError(t, err)
	before, err := first.Ref().Ask(
		ctx, &round.GetClientStateRequest{},
	).Await(ctx).Unpack()
	require.NoError(t, err)
	checkpoint, err := store.LoadCheckpoint(ctx, cfg.Name)
	require.NoError(t, err)
	require.NotNil(t, checkpoint)
	require.Equal(t, "client-round", checkpoint.StateType)
	require.NoError(t, first.StopAndWait(ctx))

	second := start()
	after, err := second.Ref().Ask(
		ctx, &round.GetClientStateRequest{},
	).Await(ctx).Unpack()
	require.NoError(t, err)
	require.Equal(t, before, after)
	state, ok := after.(*round.GetClientStateResponse)
	require.True(t, ok)
	require.Len(t, state.States, 1)
	for _, info := range state.States {
		pending, ok := info.State.(*round.PendingRoundAssembly)
		require.True(t, ok)
		require.EqualValues(t, 321, pending.VTXOs[0].Amount)
	}
	require.Empty(t, serverRef.Messages())
	require.Empty(t, timerRef.Messages())
}
