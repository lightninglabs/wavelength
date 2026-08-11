// Package oorbridge connects Ark channel actions to the durable OOR actor.
package oorbridge

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/oor"
)

const defaultPollInterval = 50 * time.Millisecond

// Controller drives the durable OOR actor and reports its terminal result back
// to the channel FSM.
type Controller struct {
	ref actor.ActorRef[oor.OORDurableMsg, oor.ActorResp]

	pollInterval time.Duration

	mu   sync.RWMutex
	sink arkchannel.ChannelEventSink
}

// New resolves the durable OOR registry from an actor system.
func New(system actor.SystemContext) (*Controller, error) {
	if system == nil {
		return nil, fmt.Errorf("actor system is required")
	}

	return NewWithRef(oor.NewServiceKey().Ref(system))
}

// NewWithRef constructs a controller from an explicit actor reference. It is
// useful for runtimes that already resolved the service.
func NewWithRef(ref actor.ActorRef[oor.OORDurableMsg, oor.ActorResp]) (
	*Controller, error) {

	if ref == nil {
		return nil, fmt.Errorf("OOR actor reference is required")
	}

	return &Controller{
		ref:          ref,
		pollInterval: defaultPollInterval,
	}, nil
}

// BindChannelEventSink connects terminal OOR observations to the channel FSM.
func (c *Controller) BindChannelEventSink(
	sink arkchannel.ChannelEventSink) error {

	if sink == nil {
		return fmt.Errorf("channel event sink is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sink != nil {
		return nil
	}
	c.sink = sink

	return nil
}

// ValidatePreparedOOR proves that the bound output exists in the exact
// deterministic OOR package and carries the expected channel policy metadata.
func (c *Controller) ValidatePreparedOOR(ctx context.Context,
	terms arkchannel.Terms, source arkchannel.VTXOBinding) error {

	if err := source.Validate(terms); err != nil {
		return err
	}
	state, err := c.state(ctx, source.OORSessionID)
	if err != nil {
		return err
	}
	prepared, ok := state.(*oor.Prepared)
	if !ok {
		return fmt.Errorf("OOR session is %T, expected prepared", state)
	}
	recipients, err := oor.ExtractArkRecipients(prepared.ArkPSBT)
	if err != nil {
		return fmt.Errorf("extract prepared OOR outputs: %w", err)
	}

	for _, recipient := range recipients {
		if recipient.OutputIndex != source.OutPoint.Index {
			continue
		}
		if recipient.Value != source.Amount ||
			!bytes.Equal(recipient.PkScript, source.PkScript) {
			return fmt.Errorf("prepared OOR output does not " +
				"match binding")
		}
		for _, declared := range prepared.RecipientOutputs {
			if declared.Value != source.Amount ||
				!bytes.Equal(
					declared.PkScript, source.PkScript,
				) {

				continue
			}
			if !bytes.Equal(
				declared.VTXOPolicyTemplate,
				source.PolicyTemplate,
			) {
				return fmt.Errorf("prepared OOR policy does " +
					"not match binding")
			}

			return nil
		}

		return fmt.Errorf("prepared OOR output has no channel policy")
	}

	return fmt.Errorf("prepared OOR output index %d is missing",
		source.OutPoint.Index)
}

// CommitPreparedOOR crosses the OOR signing gate and waits for a durable
// terminal result before advancing the channel FSM.
func (c *Controller) CommitPreparedOOR(ctx context.Context, id arkchannel.ID,
	terms arkchannel.Terms, source arkchannel.VTXOBinding) error {

	if err := source.Validate(terms); err != nil {
		return err
	}
	if err := c.drive(ctx, source, &oor.CommitPreparedEvent{}); err != nil {
		return err
	}

	return c.waitForTerminal(ctx, id, source, false)
}

// AbortPreparedOOR aborts only a pre-PONR transfer and waits until that fact is
// durable before allowing lnd funding cancellation.
func (c *Controller) AbortPreparedOOR(ctx context.Context, id arkchannel.ID,
	terms arkchannel.Terms, source arkchannel.VTXOBinding,
	reason string) error {

	if err := source.Validate(terms); err != nil {
		return err
	}
	if reason == "" {
		return fmt.Errorf("OOR abort reason is required")
	}
	if err := c.drive(ctx, source, &oor.AbortPreparedEvent{
		Reason: reason,
	}); err != nil {
		return err
	}

	return c.waitForTerminal(ctx, id, source, true)
}

// drive records one durable control event in the exact OOR session.
func (c *Controller) drive(ctx context.Context, source arkchannel.VTXOBinding,
	event oor.Event) error {

	result := c.ref.Ask(actor.WithoutTx(ctx), &oor.DriveEventRequest{
		SessionID: oor.SessionID(source.OORSessionID),
		Event:     event,
	}).Await(ctx)
	response, err := result.Unpack()
	if err != nil {
		return fmt.Errorf("drive OOR session: %w", err)
	}
	if _, ok := response.(*oor.DriveEventResponse); !ok {
		return fmt.Errorf("unexpected OOR drive response %T", response)
	}

	return nil
}

// waitForTerminal observes the OOR actor until it can produce a safe channel
// callback. A post-PONR failure is never translated into an abort.
func (c *Controller) waitForTerminal(ctx context.Context, id arkchannel.ID,
	source arkchannel.VTXOBinding, aborting bool) error {

	for {
		state, err := c.state(ctx, source.OORSessionID)
		if err != nil {
			return err
		}

		switch state := state.(type) {
		case *oor.Completed:
			if aborting {
				return fmt.Errorf("OOR finalized while " +
					"aborting channel")
			}

			return c.apply(ctx, id, &arkchannel.OORFinalized{
				SessionID: source.OORSessionID,
			})

		case *oor.Failed:
			if !state.PrePONR {
				return fmt.Errorf("OOR failed after point of "+
					"no return: %s", state.Reason)
			}

			return c.apply(ctx, id, &arkchannel.OORAborted{
				SessionID: source.OORSessionID,
				Reason:    state.Reason,
			})
		}

		timer := time.NewTimer(c.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}

			return ctx.Err()

		case <-timer.C:
		}
	}
}

// state loads the authoritative in-memory state from the durable OOR actor.
func (c *Controller) state(ctx context.Context, sessionID [32]byte) (
	oor.SessionState, error) {

	result := c.ref.Ask(actor.WithoutTx(ctx), &oor.GetStateRequest{
		SessionID: oor.SessionID(sessionID),
	}).Await(ctx)
	response, err := result.Unpack()
	if err != nil {
		return nil, fmt.Errorf("get OOR session state: %w", err)
	}
	state, ok := response.(*oor.GetStateResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected OOR state response %T",
			response)
	}
	if state.State == nil {
		return nil, fmt.Errorf("OOR session returned an empty state")
	}

	return state.State, nil
}

// apply records one OOR terminal observation through the bound channel service.
func (c *Controller) apply(ctx context.Context, id arkchannel.ID,
	event arkchannel.Event) error {

	c.mu.RLock()
	sink := c.sink
	c.mu.RUnlock()
	if sink == nil {
		return fmt.Errorf("channel event sink is not bound")
	}

	_, err := sink.Apply(ctx, id, event)

	return err
}

var _ arkchannel.OORTransferController = (*Controller)(nil)
var _ arkchannel.ChannelEventSinkBinder = (*Controller)(nil)
