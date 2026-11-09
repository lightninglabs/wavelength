package arkchannel

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// Environment names one pure Ark-channel state machine instance.
type Environment struct {
	ChannelID ID
}

// Name returns the stable state machine name.
func (e *Environment) Name() string {
	return fmt.Sprintf("ark_channel_%x", e.ChannelID[:4])
}

// State is one protofsm state carrying only Ark coordination facts.
type State interface {
	protofsm.State[Event, Action, *Environment]

	Snapshot() Snapshot

	stateSealed()
}

// StateTransition is the Ark-channel protofsm transition type.
type StateTransition = protofsm.StateTransition[
	Event, Action, *Environment,
]

// EmittedEvent is the Ark-channel protofsm emission type.
type EmittedEvent = protofsm.EmittedEvent[Event, Action]

// channelState is intentionally one compact state representation. Phase is
// durable and validated; lnd remains the richer channel state machine.
type channelState struct {
	snapshot Snapshot
}

// NewState validates immutable terms and creates the requested state.
func NewState(terms Terms) (State, error) {
	if err := terms.Validate(); err != nil {
		return nil, err
	}

	return &channelState{snapshot: Snapshot{
		Terms: terms.Clone(),
		Phase: PhaseRequested,
	}}, nil
}

// RestoreState validates and restores one durable snapshot.
func RestoreState(snapshot Snapshot) (State, error) {
	if err := validateSnapshot(snapshot); err != nil {
		return nil, err
	}

	return &channelState{snapshot: snapshot.Clone()}, nil
}

// Snapshot returns an isolated copy of the durable state.
func (s *channelState) Snapshot() Snapshot {
	return s.snapshot.Clone()
}

// String returns the durable coordination phase.
func (s *channelState) String() string {
	return s.snapshot.Phase.String()
}

// IsTerminal reports whether coordination is complete or safely abandoned.
func (s *channelState) IsTerminal() bool {
	return s.snapshot.Phase.IsTerminal()
}

func (*channelState) stateSealed() {}

// ProcessEvent applies one idempotent fact and emits only replayable actions.
func (s *channelState) ProcessEvent(_ context.Context, event Event,
	_ *Environment) (*StateTransition, error) {

	if event == nil {
		return nil, fmt.Errorf("channel event is required")
	}
	previousPhase := s.snapshot.Phase
	next := s.snapshot.Clone()
	changed, err := applyEvent(&next, event)
	if err != nil {
		return nil, err
	}
	if !changed {
		action, err := PendingAction(next)
		if err != nil {
			return nil, err
		}

		return transitionTo(next, action), nil
	}
	action, err := advance(&next)
	if err != nil {
		return nil, err
	}
	if next.Phase == previousPhase {
		action = nil
	}
	if err := validateSnapshot(next); err != nil {
		return nil, err
	}

	return transitionTo(next, action), nil
}

// PendingAction returns the idempotent side effect implied by durable state.
func PendingAction(snapshot Snapshot) (Action, error) {
	if err := validateSnapshot(snapshot); err != nil {
		return nil, err
	}
	if snapshot.Phase == PhaseNegotiating {
		return &NegotiateFunding{
			Terms:  snapshot.Terms.Clone(),
			Source: snapshot.Source.Clone(),
		}, nil
	}

	return nil, nil
}

// applyEvent mutates only the fact directly asserted by an event.
func applyEvent(next *Snapshot, event Event) (bool, error) {
	switch event := event.(type) {
	case *OORPreparationStarted:
		return applyOORPreparationStarted(next)

	case *BindVTXO:
		return applyBinding(next, event.Binding)

	default:
		return false, fmt.Errorf("unknown channel event %T", event)
	}
}

// applyOORPreparationStarted arms preparation once while immutable channel
// terms are still the only externally visible protocol fact.
func applyOORPreparationStarted(next *Snapshot) (bool, error) {
	if next.OORPreparationStarted {
		return false, nil
	}
	if next.Phase != PhaseRequested || next.Source != nil {
		return false, fmt.Errorf("cannot start OOR preparation from %s",
			next.Phase)
	}

	next.OORPreparationStarted = true

	return true, nil
}

// applyBinding binds the exact source once before native funding starts.
func applyBinding(next *Snapshot, binding VTXOBinding) (bool, error) {
	if err := binding.Validate(next.Terms); err != nil {
		return false, err
	}
	if next.Source != nil {
		if bindingsEqual(*next.Source, binding) {
			if next.OORPreparationStarted {
				return false, nil
			}
			next.OORPreparationStarted = true

			return true, nil
		}

		return false, fmt.Errorf("channel already bound to another " +
			"VTXO")
	}
	if next.Phase != PhaseRequested {
		return false, fmt.Errorf("cannot bind VTXO from %s", next.Phase)
	}
	clone := binding.Clone()
	next.Source = &clone
	next.OORPreparationStarted = true
	next.Phase = PhaseNegotiating

	return true, nil
}

// advance derives the next phase and replayable action from durable facts.
func advance(next *Snapshot) (Action, error) {
	return PendingAction(*next)
}

// transitionTo wraps one durable next state and optional action.
func transitionTo(snapshot Snapshot, action Action) *StateTransition {
	transition := &StateTransition{
		NextState: &channelState{
			snapshot: snapshot,
		},
	}
	if action != nil {
		transition.NewEvents = fn.Some(EmittedEvent{
			Outbox: []Action{action},
		})
	}

	return transition
}

// validateSnapshot rejects impossible persisted combinations.
func validateSnapshot(snapshot Snapshot) error {
	if err := snapshot.Terms.Validate(); err != nil {
		return err
	}
	if snapshot.Source != nil {
		if err := snapshot.Source.Validate(snapshot.Terms); err != nil {
			return err
		}
	}
	switch snapshot.Phase {
	case PhaseRequested:
		if snapshot.Source != nil {
			return fmt.Errorf("requested channel has advanced " +
				"facts")
		}

	case PhaseNegotiating:
		if snapshot.Source == nil {
			return fmt.Errorf("negotiating channel has no bound " +
				"VTXO")
		}

	default:
		return fmt.Errorf("unknown channel phase %d", snapshot.Phase)
	}

	return nil
}

// bindingsEqual checks idempotency for an immutable VTXO binding.
func bindingsEqual(a, b VTXOBinding) bool {
	return a.OutPoint == b.OutPoint && a.Amount == b.Amount &&
		a.OORSessionID == b.OORSessionID &&
		string(a.PolicyTemplate) == string(b.PolicyTemplate) &&
		string(a.PkScript) == string(b.PkScript)
}
