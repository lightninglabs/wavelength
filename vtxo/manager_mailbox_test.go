package vtxo

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/timeout"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestAutoRefreshChildManagerMailboxCycle holds the manager turn while a
// real child reserves itself behind a full manager mailbox. Cohort adoption
// must finish without waiting for the five-second child Ask timeout, and the
// child's original relay must be consumed exactly once after pressure clears.
func TestAutoRefreshChildManagerMailboxCycle(t *testing.T) {
	t.Parallel()
	const height = int32(800)
	leader := deterministicCohortDescriptor(t, 1, 1000)
	sibling := deterministicCohortDescriptor(t, 2, 1000)
	persisted := make(chan struct{})
	entered := make(chan struct{})
	gate := make(chan struct{})
	finished := make(chan error, 1)
	relayed := make(chan struct{}, 1)
	var wg sync.WaitGroup
	timer := timeout.NewActor()
	timerActor := actor.NewActor(actor.ActorConfig[
		timeout.Msg,
		timeout.Resp,
	]{
		ID: "cohort-timer", Behavior: timer, MailboxSize: 1, Wg: &wg,
	})
	timer.Start(timerActor.TellRef())
	timerActor.Start()
	t.Cleanup(timerActor.Stop)
	store := &MockVTXOStore{}
	store.On("UpdateVTXOStatus", mock.Anything, sibling.Outpoint,
		VTXOStatusPendingForfeit).Run(func(mock.Arguments) {
		close(persisted)
	}).Return(nil).Once()
	expectCohortListings(store, nil, []*Descriptor{sibling})
	rounds := newMockRoundActorRef(t)
	mgr := NewManager(&ManagerConfig{Store: store, RoundActor: rounds})
	leaderRequest := autoRefreshLeaderRequest(leader, height)
	manager := actor.NewActor(actor.ActorConfig[ManagerMsg, ManagerResp]{
		ID: "cohort-manager", MailboxSize: 1, Wg: &wg,
		Behavior: actor.NewFunctionBehavior(func(ctx context.Context,
			msg ManagerMsg) fn.Result[ManagerResp] {

			relay, ok := msg.(*RelayToRoundMsg)
			if ok && relay.Payload == leaderRequest {
				close(entered)
				select {
				case <-gate:
				case <-ctx.Done():
					return fn.Err[ManagerResp](ctx.Err())
				}
				result := mgr.Receive(ctx, msg)
				_, err := result.Unpack()
				finished <- err

				return result
			}
			result := mgr.Receive(ctx, msg)
			_, deferred := msg.(*deferredRefreshRelay)
			if ok || deferred {
				relayed <- struct{}{}
			}

			return result
		}),
	})
	child := NewVTXOActor(t.Context(), &VTXOActorConfig{
		VTXO:         sibling,
		Store:        store,
		ExpiryConfig: DefaultExpiryConfig(),
		Manager:      manager.TellRef(),
		TimeoutActor: timerActor.TellRef(),
	})
	childActor := actor.NewActor(actor.ActorConfig[
		actormsg.VTXOActorMsg,
		actormsg.VTXOActorResp,
	]{
		ID: "cohort-child", Behavior: child, MailboxSize: 1, Wg: &wg,
	})
	mgr.actors[sibling.Outpoint] = childActor.Ref()
	t.Cleanup(
		func() {
			manager.Stop()
			childActor.Stop()
			timerActor.Stop()
			wg.Wait()
		},
	)
	manager.Start()
	childActor.Start()
	require.NoError(
		t,
		manager.Ref().Tell(t.Context(), &RelayToRoundMsg{
			Payload: leaderRequest,
		},
		),
	)
	<-entered
	require.NoError(
		t,
		manager.Ref().TryTell(
			t.Context(),
			&GetActiveVTXOCountRequest{},
		),
	)
	require.NoError(
		t,
		childActor.Ref().Tell(t.Context(), &BlockEpochEvent{
			Height: height,
		},
		),
	)
	<-persisted
	close(gate)
	select {
	case err := <-finished:
		require.NoError(t, err)

	case <-time.After(time.Second):
		t.Fatal(
			"manager waited on child blocked sending its " +
				"refresh relay",
		)
	}
	select {
	case <-relayed:
	case <-time.After(3 * time.Second):
		t.Fatal("retained sibling relay did not drain")
	}
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), 100*time.Millisecond,
		)
		defer cancel()
		_, err := actor.Probe(ctx, manager.Ref())

		return err == nil && len(rounds.getMessages()) == 1
	}, 3*time.Second, 10*time.Millisecond)
	messages := rounds.getMessages()
	cohort, ok := messages[0].(*round.RefreshVTXOCohortRequest)
	require.True(t, ok)
	require.Len(t, cohort.Requests, 2)
	store.AssertExpectations(t)
}
