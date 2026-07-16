package waved

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/virtualchannel"
	vcunrollpolicy "github.com/lightninglabs/wavelength/virtualchannel/unrollpolicy"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

type materializerRegistryRef struct {
	status   *unroll.GetStatusResp
	detailed bool
}

// ID returns the test actor identifier.
func (r *materializerRegistryRef) ID() string {
	return "materializer-registry"
}

// Tell satisfies actor.ActorRef.
func (r *materializerRegistryRef) Tell(_ context.Context,
	_ unroll.RegistryMsg) error {

	return nil
}

// Ask records whether the materializer requested live child state.
func (r *materializerRegistryRef) Ask(_ context.Context,
	msg unroll.RegistryMsg) actor.Future[unroll.RegistryResp] {

	promise := actor.NewPromise[unroll.RegistryResp]()
	request, ok := msg.(*unroll.GetStatusRequest)
	if !ok {
		promise.Complete(
			fn.Err[unroll.RegistryResp](
				context.Canceled,
			),
		)

		return promise.Future()
	}

	r.detailed = request.Detailed
	var response unroll.RegistryResp = r.status
	promise.Complete(fn.Ok(response))

	return promise.Future()
}

// TestVirtualChannelMaterializerWaitsForStartupDependencies pins the
// integrated-lnd startup contract: an early publish hook blocks until actor
// wiring is ready or its bounded context ends.
func TestVirtualChannelMaterializerWaitsForStartupDependencies(t *testing.T) {
	t.Parallel()

	materializer := &virtualChannelBackingMaterializer{
		manager: func() (actor.ActorRef[
			vtxo.ManagerMsg, vtxo.ManagerResp,
		], bool) {

			return nil, false
		},
		registry: func() (actor.ActorRef[
			unroll.RegistryMsg, unroll.RegistryResp,
		], bool) {

			return nil, false
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := materializer.waitForDependencies(ctx)
		done <- err
	}()

	select {
	case err := <-done:
		require.Failf(t, "dependency wait returned early", "%v", err)

	case <-time.After(20 * time.Millisecond):
	}

	cancel()
	err := <-done
	require.ErrorIs(t, err, context.Canceled)
}

// TestVirtualChannelMaterializerUsesDetailedUnrollState verifies an active
// child can release lnd's publish call before the registry writes a terminal
// coarse record.
func TestVirtualChannelMaterializerUsesDetailedUnrollState(t *testing.T) {
	t.Parallel()

	id := virtualchannel.ID{1}
	policyRef := vcunrollpolicy.EncodeVirtualChannelID(id)
	target := wire.OutPoint{Index: 1}
	ref := &materializerRegistryRef{
		status: &unroll.GetStatusResp{
			Found:  true,
			Active: true,
			Phase:  unroll.PhasePending,
			ExitPolicyKind: vcunrollpolicy.
				VirtualChannelBackingExitPolicyKind,
			ExitPolicyRef: policyRef,
			State: &unroll.GetStateResp{
				Phase: unroll.PhaseSweepConfirmation,
				ExitPolicyKind: vcunrollpolicy.
					VirtualChannelBackingExitPolicyKind,
				ExitPolicyRef: policyRef,
			},
		},
	}

	materializer := &virtualChannelBackingMaterializer{}
	ready, err := materializer.backingSpendReady(
		t.Context(), ref, target, id,
	)
	require.NoError(t, err)
	require.True(t, ready)
	require.True(t, ref.detailed)
}
