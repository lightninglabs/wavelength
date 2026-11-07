package vtxo

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// MaxMaintenanceActors bounds manager lookups and child turns per pass.
const MaxMaintenanceActors = 64

// ResolveMaintenanceActorsRequest selects existing owners only. It performs
// no startup repair, spawning, chain query or child ask in the manager turn.
type ResolveMaintenanceActorsRequest struct {
	actor.BaseMessage
	Outpoints []wire.OutPoint
}

// MessageType identifies a bounded warm maintenance lookup.
func (*ResolveMaintenanceActorsRequest) MessageType() string {
	return "ResolveMaintenanceActors"
}

// VTXOManagerMsg admits the lookup to the owning manager.
func (*ResolveMaintenanceActorsRequest) VTXOManagerMsg() {}

// maintenanceTipEvent distinguishes a sampled tip from a real chain epoch.
// A newer delivered block must not be walked back by a delayed sample.
// Equal heights remain useful for the existing two-turn expired reclaim.
type maintenanceTipEvent struct{ BlockEpochEvent }

// maintenanceActors holds only the requested live references. The external
// caller drives children while the manager continues draining their relays.
type maintenanceActors struct {
	refs    map[wire.OutPoint]VTXOActorRef
	timeout time.Duration
}

// VTXOManagerResp marks the private actor-reference result.
func (*maintenanceActors) VTXOManagerResp() {}

// resolveMaintenanceActors snapshots at most one bounded page of owners.
func (m *Manager) resolveMaintenanceActors(
	request *ResolveMaintenanceActorsRequest) fn.Result[ManagerResp] {

	if len(request.Outpoints) > MaxMaintenanceActors {
		return fn.Err[ManagerResp](
			fmt.Errorf("maintenance actor page exceeds limit"),
		)
	}
	refs := make(map[wire.OutPoint]VTXOActorRef)
	for _, point := range request.Outpoints {
		if ref, ok := m.actors[point]; ok {
			refs[point] = ref
		}
	}

	return fn.Ok[ManagerResp](
		&maintenanceActors{
			refs:    refs,
			timeout: m.forfeitVTXOActorAskTimeout(),
		},
	)
}

// ExpiryMaintenanceResult reports actual completed attempts, not global idle.
// Next counts scanned IDs, including historical rows without live actors.
type ExpiryMaintenanceResult struct {
	Next     int
	Checked  int
	Deferred bool
}

// awaitMaintenance joins the actual reply even after caller cancellation.
// Receive observes cancellation, but its final access can outlive Await(ctx).
// No untracked goroutine or timeout result releases the retained daemon owner.
func awaitMaintenance[T any](ctx context.Context,
	future actor.Future[T]) fn.Result[T] {

	result := future.Await(ctx)
	if ctx.Err() != nil {
		return future.Await(context.WithoutCancel(ctx))
	}

	return result
}

// askMaintenance retains a child future through its final reply, applying
// the manager's existing forfeit ask budget without treating timeout as a join.
func askMaintenance(ctx context.Context, ref VTXOActorRef,
	epoch *BlockEpochEvent,
	timeout time.Duration) fn.Result[actormsg.VTXOActorResp] {

	askCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		askCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	return awaitMaintenance(
		askCtx,
		ref.Ask(
			askCtx, &maintenanceTipEvent{
				BlockEpochEvent: *epoch,
			},
		),
	)
}

// MaintainExpiry applies one sampled current tip to existing owners outside
// the manager turn. Existing FSM policy may refresh, retain ownership, begin a
// critical exit or persist expiry. It does not repeat startup repair.
// The caller must retain its daemon through return, including canceled joins.
func MaintainExpiry(ctx context.Context,
	manager actor.ActorRef[ManagerMsg, ManagerResp],
	outpoints []wire.OutPoint,
	epoch BlockEpochEvent) (ExpiryMaintenanceResult, error) {

	var progress ExpiryMaintenanceResult
	if len(outpoints) > MaxMaintenanceActors {
		return progress, fmt.Errorf("maintenance actor page exceeds " +
			"limit")
	}
	if err := ctx.Err(); err != nil {
		return progress, err
	}
	value, err := awaitMaintenance(ctx, manager.Ask(
		ctx, &ResolveMaintenanceActorsRequest{
			Outpoints: outpoints,
		},
	)).Unpack()
	if err != nil {
		return progress, err
	}
	plan, ok := value.(*maintenanceActors)
	if !ok {
		return progress, fmt.Errorf("unexpected maintenance actor " +
			"response")
	}
	for _, point := range outpoints {
		if err := ctx.Err(); err != nil {
			return progress, err
		}
		ref, owned := plan.refs[point]
		if !owned {
			progress.Next++
			continue
		}
		response, err := askMaintenance(
			ctx, ref, &epoch, plan.timeout,
		).Unpack()
		if err := ctx.Err(); err != nil {
			return progress, err
		}
		if err != nil {
			progress.Next++
			progress.Deferred = true
			continue
		}
		transition, ok := response.(VTXOActorResponse)
		if !ok {
			progress.Next++
			progress.Deferred = true
			continue
		}
		progress.Checked++
		_, becameExpired := transition.NewState.(*ExpiredState)
		_, wasExpired := transition.PriorState.(*ExpiredState)
		if becameExpired && !wasExpired {
			_, err = askMaintenance(
				ctx, ref, &epoch, plan.timeout,
			).Unpack()
			if err := ctx.Err(); err != nil {
				return progress, err
			}
			if err != nil {
				progress.Deferred = true
			}
		}
		progress.Next++
	}

	return progress, nil
}

// Ensure the message remains an ordinary manager query, never a signing RPC.
var _ actormsg.VTXOManagerMsg = (*ResolveMaintenanceActorsRequest)(nil)
