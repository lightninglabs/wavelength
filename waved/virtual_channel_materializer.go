package waved

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/virtualchannel"
	vcpolicy "github.com/lightninglabs/wavelength/virtualchannel/unrollpolicy"
)

const (
	virtualChannelMaterializationPoll = 500 * time.Millisecond
	virtualChannelMaterializationWait = 30 * time.Minute
)

// virtualChannelBackingMaterializer starts a VTXO unroll job that uses the
// signed cooperative backing transaction as its terminal spend, then blocks
// until that terminal spend has been submitted to txconfirm.
type virtualChannelBackingMaterializer struct {
	registry func() (actor.ActorRef[
		unroll.RegistryMsg, unroll.RegistryResp,
	], bool)

	pollInterval time.Duration
	timeout      time.Duration
}

// newVirtualChannelBackingMaterializer creates the lnd publish-hook adapter
// for virtual-channel conflict materialization.
func newVirtualChannelBackingMaterializer(
	registry func() (actor.ActorRef[
		unroll.RegistryMsg, unroll.RegistryResp,
	], bool)) *virtualChannelBackingMaterializer {

	return &virtualChannelBackingMaterializer{
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

	ref, ok := m.registryRef()
	if !ok {
		return fmt.Errorf("unroll registry is not initialized")
	}

	ctx, cancel := m.withTimeout(ctx)
	defer cancel()

	target := channel.BackingVTXOs[0].OutPoint
	_, err := m.ensureUnroll(ctx, ref, target, channel.ID)
	if err != nil {
		return err
	}

	return m.waitForBackingSpend(ctx, ref, target, channel.ID)
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

// ensureUnroll admits or deduplicates the cooperative materialization job.
func (m *virtualChannelBackingMaterializer) ensureUnroll(ctx context.Context,
	ref actor.ActorRef[unroll.RegistryMsg, unroll.RegistryResp],
	target wire.OutPoint, id virtualchannel.ID) (*unroll.EnsureUnrollResp,
	error) {

	resp, err := ref.Ask(ctx, &unroll.EnsureUnrollRequest{
		Outpoint:       target,
		Trigger:        unroll.TriggerManual,
		ExitPolicyKind: vcpolicy.VirtualChannelBackingExitPolicyKind,
		ExitPolicyRef:  vcpolicy.EncodeVirtualChannelID(id),
	}).Await(ctx).Unpack()
	if err != nil {
		return nil, fmt.Errorf("ensure virtual channel backing "+
			"unroll: %w", err)
	}

	ensureResp, ok := resp.(*unroll.EnsureUnrollResp)
	if !ok {
		return nil, fmt.Errorf("unexpected unroll ensure response %T",
			resp)
	}

	return ensureResp, nil
}

// waitForBackingSpend waits until the unroll actor has submitted the
// cooperative backing spend to txconfirm.
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
		done, err := m.backingSpendSubmitted(ctx, ref, target, id)
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

// backingSpendSubmitted checks whether the unroll registry has reached the
// phase where lnd's child spend may be published.
func (m *virtualChannelBackingMaterializer) backingSpendSubmitted(
	ctx context.Context,
	ref actor.ActorRef[unroll.RegistryMsg, unroll.RegistryResp],
	target wire.OutPoint, id virtualchannel.ID) (bool, error) {

	resp, err := ref.Ask(ctx, &unroll.GetStatusRequest{
		Outpoint: target,
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
		return false, fmt.Errorf("virtual channel backing unroll not "+
			"found for %v", target)
	}
	if status.ExitPolicyKind !=
		vcpolicy.VirtualChannelBackingExitPolicyKind {
		return false, fmt.Errorf("unroll job for %v has policy %s, "+
			"expected %s", target, status.ExitPolicyKind,
			vcpolicy.VirtualChannelBackingExitPolicyKind)
	}
	if status.ExitPolicyRef != vcpolicy.EncodeVirtualChannelID(id) {
		return false, fmt.Errorf("unroll job for %v has policy ref "+
			"%q, expected %q", target, status.ExitPolicyRef,
			vcpolicy.EncodeVirtualChannelID(id))
	}
	if status.Phase == unroll.PhaseFailed {
		return false, fmt.Errorf("virtual channel backing unroll "+
			"failed: %s", status.FailReason)
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
