package oorbridge

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/oor"
)

const defaultPollInterval = 50 * time.Millisecond

// Controller prepares and validates channel-policy outputs through the durable
// OOR actor before the funding protocol may release signatures.
type Controller struct {
	ref          actor.ActorRef[oor.OORDurableMsg, oor.ActorResp]
	pollInterval time.Duration
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

// state loads the authoritative state through the durable OOR registry.
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
