package waved

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/virtualchannel"
	vcunrollpolicy "github.com/lightninglabs/wavelength/virtualchannel/unrollpolicy"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	virtualChannelMaterializationPoll = 500 * time.Millisecond
	virtualChannelMaterializationWait = 30 * time.Minute
	virtualChannelDependencyPoll      = 100 * time.Millisecond
)

// virtualChannelBackingMaterializer starts a VTXO unroll job that uses the
// signed cooperative backing transaction as its terminal spend, then blocks
// until the unroll registry reports that the terminal spend may be built upon.
type virtualChannelBackingMaterializer struct {
	manager func() (actor.ActorRef[
		vtxo.ManagerMsg, vtxo.ManagerResp,
	], bool)

	registry func() (actor.ActorRef[
		unroll.RegistryMsg, unroll.RegistryResp,
	], bool)

	pollInterval time.Duration
	timeout      time.Duration
}

// newVirtualChannelBackingMaterializer creates the lnd publish-hook adapter
// for virtual-channel conflict materialization.
func newVirtualChannelBackingMaterializer(
	manager func() (actor.ActorRef[
		vtxo.ManagerMsg, vtxo.ManagerResp,
	], bool),
	registry func() (actor.ActorRef[
		unroll.RegistryMsg, unroll.RegistryResp,
	], bool)) *virtualChannelBackingMaterializer {

	return &virtualChannelBackingMaterializer{
		manager:      manager,
		registry:     registry,
		pollInterval: virtualChannelMaterializationPoll,
		timeout:      virtualChannelMaterializationWait,
	}
}

// MaterializeVirtualChannelBacking implements
// virtualchannel.BackingMaterializer.
func (m *virtualChannelBackingMaterializer) MaterializeVirtualChannelBacking(
	ctx context.Context, channel *virtualchannel.Channel) error {

	if channel == nil {
		return fmt.Errorf("virtual channel must be provided")
	}
	if len(channel.BackingVTXOs) != 1 {
		return fmt.Errorf("virtual channel conflict materialization "+
			"requires exactly one backing VTXO, got %d",
			len(channel.BackingVTXOs))
	}
	if m == nil || m.manager == nil {
		return fmt.Errorf("VTXO manager resolver is not configured")
	}
	if m.registry == nil {
		return fmt.Errorf("unroll registry resolver is not configured")
	}

	ctx, cancel := m.withTimeout(ctx)
	defer cancel()
	managerRef, registryRef, err := m.waitForDependencies(ctx)
	if err != nil {
		return err
	}

	target := channel.BackingVTXOs[0].OutPoint
	if err := m.requestUnroll(
		ctx, managerRef, target, channel.ID,
	); err != nil {
		return err
	}

	return m.waitForBackingSpend(
		ctx, registryRef, target, channel.ID,
	)
}

func (m *virtualChannelBackingMaterializer) managerRef() (
	actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp], bool) {

	if m == nil || m.manager == nil {
		return nil, false
	}

	return m.manager()
}

// registryRef resolves the current unroll registry ref.
func (m *virtualChannelBackingMaterializer) registryRef() (
	actor.ActorRef[unroll.RegistryMsg, unroll.RegistryResp], bool) {

	if m == nil || m.registry == nil {
		return nil, false
	}

	return m.registry()
}

// withTimeout bounds lnd's blocking publish hook while respecting a shorter
// caller deadline.
func (m *virtualChannelBackingMaterializer) withTimeout(ctx context.Context) (
	context.Context, context.CancelFunc) {

	timeout := m.timeout
	if timeout <= 0 {
		timeout = virtualChannelMaterializationWait
	}

	return context.WithTimeout(ctx, timeout)
}

// waitForDependencies bridges integrated lnd's startup ordering. The publish
// hook can run before Wavelength has started the VTXO and unroll actors, so it
// waits within the materialization deadline instead of relying on lnd to retry
// a transient initialization error.
func (m *virtualChannelBackingMaterializer) waitForDependencies(
	ctx context.Context) (actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp],
	actor.ActorRef[unroll.RegistryMsg, unroll.RegistryResp], error) {

	ticker := time.NewTicker(virtualChannelDependencyPoll)
	defer ticker.Stop()

	for {
		managerRef, managerReady := m.managerRef()
		registryRef, registryReady := m.registryRef()
		if managerReady && registryReady {
			return managerRef, registryRef, nil
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("wait for virtual channel "+
				"materialization dependencies: %w", ctx.Err())
		}
	}
}

// requestUnroll moves the backing VTXO through its owning FSM. The actor's
// outbox then admits the registry job with the channel-specific final spend.
func (m *virtualChannelBackingMaterializer) requestUnroll(ctx context.Context,
	ref actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp],
	target wire.OutPoint, id virtualchannel.ID) error {

	resp, err := ref.Ask(ctx, &actormsg.ForceUnrollRequest{
		Outpoint: target,
		Reason:   "materialize virtual channel backing",
		Trigger:  actormsg.UnrollTriggerManual,
		ExitPolicy: fn.Some(actormsg.ExitPolicy{
			Kind: actormsg.ExitPolicyVirtualChannelBacking,
			Ref: actormsg.ExitPolicyRef(
				vcunrollpolicy.EncodeVirtualChannelID(id),
			),
		}),
	}).Await(ctx).Unpack()
	if err != nil {
		return fmt.Errorf("request virtual channel backing unroll: %w",
			err)
	}

	forceResp, ok := resp.(*actormsg.ForceUnrollResponse)
	if !ok {
		return fmt.Errorf("unexpected VTXO unroll response %T", resp)
	}
	if !forceResp.Accepted {
		return fmt.Errorf("VTXO unroll was not accepted: %s",
			forceResp.Reason)
	}

	return nil
}

// waitForBackingSpend waits until the coarse durable unroll state proves that
// lnd may safely publish a child of the cooperative backing spend.
func (m *virtualChannelBackingMaterializer) waitForBackingSpend(
	ctx context.Context,
	ref actor.ActorRef[unroll.RegistryMsg, unroll.RegistryResp],
	target wire.OutPoint, id virtualchannel.ID) error {

	interval := m.pollInterval
	if interval <= 0 {
		interval = virtualChannelMaterializationPoll
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		done, err := m.backingSpendReady(ctx, ref, target, id)
		if err != nil {
			return err
		}
		if done {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for virtual channel backing "+
				"materialization: %w", ctx.Err())

		case <-ticker.C:
		}
	}
}

// backingSpendReady checks whether the unroll registry has reached the
// phase where lnd's child spend may be published.
func (m *virtualChannelBackingMaterializer) backingSpendReady(
	ctx context.Context,
	ref actor.ActorRef[unroll.RegistryMsg, unroll.RegistryResp],
	target wire.OutPoint, id virtualchannel.ID) (bool, error) {

	resp, err := ref.Ask(ctx, &unroll.GetStatusRequest{
		Outpoint: target,
		Detailed: true,
	}).Await(ctx).Unpack()
	if err != nil {
		return false, fmt.Errorf("query virtual channel backing "+
			"unroll: %w", err)
	}

	status, ok := resp.(*unroll.GetStatusResp)
	if !ok {
		return false, fmt.Errorf("unexpected unroll status response %T",
			resp)
	}
	if !status.Found {

		// The manager owns admission and its VTXO actor forwards the
		// registry message asynchronously. Keep polling through that
		// short handoff.
		return false, nil
	}
	expectedPolicy := vcunrollpolicy.VirtualChannelBackingExitPolicyKind
	expectedRef := vcunrollpolicy.EncodeVirtualChannelID(id)
	if status.ExitPolicyKind != expectedPolicy {
		return false, fmt.Errorf("unroll job for %v has policy %s, "+
			"expected %s", target, status.ExitPolicyKind,
			expectedPolicy)
	}
	if status.ExitPolicyRef != expectedRef {
		return false, fmt.Errorf("unroll job for %v has policy ref "+
			"%q, expected %q", target, status.ExitPolicyRef,
			expectedRef)
	}
	if status.Phase == unroll.PhaseFailed {
		return false, fmt.Errorf("virtual channel backing unroll "+
			"failed: %s", status.FailReason)
	}

	// The registry's coarse record is intentionally stable while a child
	// actor is active. Its detailed state is the authoritative live view of
	// whether the cooperative terminal spend has reached txconfirm.
	if status.State != nil {
		if status.State.ExitPolicyKind != expectedPolicy {
			return false, fmt.Errorf("active unroll job for %v "+
				"has policy %s, expected %s", target,
				status.State.ExitPolicyKind, expectedPolicy)
		}
		if status.State.ExitPolicyRef != expectedRef {
			return false, fmt.Errorf("active unroll job for %v "+
				"has policy ref %q, expected %q", target,
				status.State.ExitPolicyRef, expectedRef)
		}
		if status.State.Phase == unroll.PhaseFailed {
			return false, fmt.Errorf("virtual channel backing "+
				"unroll failed: %s", status.State.FailReason)
		}
		if status.State.SweepTxid != nil ||
			virtualChannelBackingPublished(status.State.Phase) {
			return true, nil
		}
	}
	if status.SweepTxid != nil ||
		virtualChannelBackingPublished(status.Phase) {
		return true, nil
	}

	return false, nil
}

// virtualChannelBackingPublished reports whether the unroll job has advanced
// past final-spend submission and lnd may publish its child spend.
func virtualChannelBackingPublished(phase unroll.Phase) bool {
	switch phase {
	case unroll.PhaseSweepConfirmation, unroll.PhaseCompleted:
		return true

	default:
		return false
	}
}

// Compile-time check.
type vcBackingMaterializer = *virtualChannelBackingMaterializer

var _ virtualchannel.BackingMaterializer = vcBackingMaterializer(nil)
