package vtxo

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestMaintainExpiryUsesExistingOwners applies a current tip without opening
// the startup gate, reading history, repairing forfeits or spawning children.
func TestMaintainExpiryUsesExistingOwners(t *testing.T) {
	expired := makeDescriptor(t, 50_000, 0)
	safe := makeDescriptor(t, 40_000, 1)
	tip := expired.BatchExpiry
	safe.BatchExpiry = tip + 1000
	manager := NewManager(
		&ManagerConfig{
			DeferAutomaticRefreshUntilRoundReady: true,
		},
	)
	expiredRef := &maintenanceTestRef{newMockVTXOActorRef(
		"expired", &LiveState{
			VTXO: expired,
		},
	)}
	safeRef := &maintenanceTestRef{newMockVTXOActorRef(
		"safe", &LiveState{
			VTXO: safe,
		},
	)}
	manager.actors[expired.Outpoint] = expiredRef
	manager.actors[safe.Outpoint] = safeRef
	loop := actor.NewActor(
		actor.ActorConfig[ManagerMsg, ManagerResp]{
			ID:       "maintenance-manager",
			Behavior: manager,
		},
	)
	loop.Start()
	t.Cleanup(loop.Stop)
	progress, err := MaintainExpiry(
		t.Context(), loop.Ref(),
		[]wire.OutPoint{expired.Outpoint, safe.Outpoint, {}},
		BlockEpochEvent{
			Height: tip,
		},
	)
	require.NoError(t, err)
	require.Equal(t, 3, progress.Next)
	require.Equal(t, 2, progress.Checked)
	require.False(t, progress.Deferred)
	require.True(t, manager.roundStartupPending)
	require.IsType(
		t, &PendingForfeitState{},
		expiredRef.state,
	)
	require.IsType(
		t, &LiveState{},
		safeRef.state,
	)
}

// TestMaintenanceCancellationJoinsReply proves cancellation cannot release
// the retained helper before the child has actually completed its final access.
func TestMaintenanceCancellationJoinsReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		promise := actor.NewPromise[int]()
		done := make(chan fn.Result[int], 1)
		go func() {
			done <- awaitMaintenance(ctx, promise.Future())
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal(
				"helper returned before the child's actual " +
					"reply",
			)

		default:
		}
		promise.Complete(fn.Ok(7))
		value, err := (<-done).Unpack()
		require.NoError(t, err)
		require.Equal(t, 7, value)
	})
}

// maintenanceTestRef drives the existing FSM double with sampled-tip envelopes.
type maintenanceTestRef struct{ *mockVTXOActorRef }

// Ask unwraps maintenance metadata; the real actor's stale-tip fence is tested
// independently below, while the FSM double observes the ordinary epoch.
func (m *maintenanceTestRef) Ask(ctx context.Context,
	msg actormsg.VTXOActorMsg) actor.Future[actormsg.VTXOActorResp] {

	if sample, ok := msg.(*maintenanceTipEvent); ok {
		msg = &sample.BlockEpochEvent
	}

	return m.mockVTXOActorRef.Ask(ctx, msg)
}

// TestMaintenanceSampleCannotUndoNewerBlock proves a delayed sampled tip is
// never interpreted as a reorg. Real block epochs retain their existing path.
func TestMaintenanceSampleCannotUndoNewerBlock(t *testing.T) {
	desc := makeDescriptor(t, 50_000, 0)
	state := &ExpiredState{VTXO: desc}
	owner := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO: desc,
		},
		state:           state,
		lastEpochHeight: desc.BatchExpiry,
	}
	response, err := owner.Receive(t.Context(), &maintenanceTipEvent{
		BlockEpochEvent: BlockEpochEvent{Height: desc.BatchExpiry - 1},
	}).Unpack()
	require.NoError(t, err)
	transition, ok := response.(VTXOActorResponse)
	require.True(t, ok)
	require.Same(t, state, transition.NewState)
}
