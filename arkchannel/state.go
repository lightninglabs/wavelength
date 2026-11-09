package arkchannel

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
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
	_, oorFinalized := event.(*OORFinalized)
	if next.Phase == previousPhase && !oorFinalized {
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
	switch snapshot.Phase {
	case PhaseNegotiating:
		return &NegotiateFunding{
			Terms:  snapshot.Terms.Clone(),
			Source: snapshot.Source.Clone(),
		}, nil

	case PhaseBackingReady:
		if snapshot.OORFinalized {
			return &PrepareRecovery{
				Terms:  snapshot.Terms.Clone(),
				Source: snapshot.Source.Clone(),
			}, nil
		}

		return &CommitOOR{
			Terms:  snapshot.Terms.Clone(),
			Source: snapshot.Source.Clone(),
		}, nil

	case PhaseActivating:
		return &ActivateChannel{
			Terms:   snapshot.Terms.Clone(),
			Backing: snapshot.Backing.Clone(),
		}, nil

	default:
		return nil, nil
	}
}

// applyEvent mutates only the fact directly asserted by an event.
func applyEvent(next *Snapshot, event Event) (bool, error) {
	switch event := event.(type) {
	case *OORPreparationStarted:
		return applyOORPreparationStarted(next)

	case *BindVTXO:
		return applyBinding(next, event.Binding)

	case *BackingSigned:
		return applyBacking(next, event.Backing)

	case *FundingFinalized:
		return applyFundingFinalized(next, event.Party)

	case *OORFinalized:
		return applyOORFinalized(next, event.SessionID)

	case *RecoveryPackageInstalled:
		return applyRecoveryPackageInstalled(next, event.Party)

	case *ChannelActive:
		return applyChannelActive(
			next, wire.OutPoint{
				Hash:  event.ChannelPointHash,
				Index: event.ChannelPointIndex,
			},
		)

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

// applyFundingFinalized records one native lnd finalization acknowledgement.
func applyFundingFinalized(next *Snapshot, party Party) (bool, error) {
	if next.Source == nil {
		return false, fmt.Errorf("cannot finalize funding before " +
			"VTXO binding")
	}
	if next.Backing == nil {
		return false, fmt.Errorf("cannot finalize funding before " +
			"signed backing")
	}

	switch party {
	case PartyClient:
		if next.ClientFinalized {
			return false, nil
		}
		next.ClientFinalized = true

	case PartyHub:
		if next.HubFinalized {
			return false, nil
		}
		next.HubFinalized = true

	default:
		return false, fmt.Errorf("unknown funding party %d", party)
	}

	return true, nil
}

// applyBacking validates and records the immutable signed transaction.
func applyBacking(next *Snapshot, backing Backing) (bool, error) {
	if next.Source == nil {
		return false, fmt.Errorf("cannot record backing before VTXO " +
			"binding")
	}
	if err := backing.Validate(next.Terms, *next.Source); err != nil {
		return false, err
	}
	if next.Backing != nil {
		if backingsEqual(*next.Backing, backing) {
			return false, nil
		}

		return false, fmt.Errorf("channel already has another backing")
	}

	clone := backing.Clone()
	next.Backing = &clone

	return true, nil
}

// applyOORFinalized records completion of the exact prepared transfer. The
// state machine can activate lnd only after this fact is durable.
func applyOORFinalized(next *Snapshot, sessionID [32]byte) (bool, error) {
	if next.Source == nil {
		return false, fmt.Errorf("cannot finalize OOR before VTXO " +
			"binding")
	}
	if next.Source.OORSessionID != sessionID {
		return false, fmt.Errorf("finalized OOR session does not " +
			"match channel source")
	}
	if next.OORFinalized {
		return false, nil
	}
	if next.Phase != PhaseBackingReady || !backingReady(*next) {
		return false, fmt.Errorf("cannot finalize OOR from %s",
			next.Phase)
	}

	next.OORFinalized = true

	return true, nil
}

// applyRecoveryPackageInstalled records the symmetric recovery barrier. It is
// valid before the remote OOR-finalized acknowledgement arrives because the
// package itself can only be exported after actual OOR completion.
func applyRecoveryPackageInstalled(next *Snapshot, party Party) (bool, error) {
	if next.Source == nil || next.Backing == nil || !backingReady(*next) {
		return false, fmt.Errorf("cannot install channel recovery " +
			"before finalized backing")
	}
	switch party {
	case PartyClient:
		if next.ClientRecoveryReady {
			return false, nil
		}
		next.ClientRecoveryReady = true

	case PartyHub:
		if next.HubRecoveryReady {
			return false, nil
		}
		next.HubRecoveryReady = true

	default:
		return false, fmt.Errorf("unknown recovery party %d", party)
	}

	return true, nil
}

// applyChannelActive verifies lnd activated the negotiated funding output.
func applyChannelActive(next *Snapshot,
	channelPoint wire.OutPoint) (bool, error) {

	if next.Phase == PhaseActive &&
		next.Backing.ChannelPoint == channelPoint {
		return false, nil
	}
	if next.Phase != PhaseActivating {
		return false, fmt.Errorf("cannot activate channel from %s",
			next.Phase)
	}
	if next.Backing.ChannelPoint != channelPoint {
		return false, fmt.Errorf("lnd activated an unexpected " +
			"channel point")
	}
	next.Phase = PhaseActive

	return true, nil
}

// advance derives the next phase and replayable action from durable facts.
func advance(next *Snapshot) (Action, error) {
	if next.Phase == PhaseNegotiating && backingReady(*next) {
		next.Phase = PhaseBackingReady
	}
	if next.Phase == PhaseBackingReady && next.OORFinalized &&
		next.RecoveryReady() {

		next.Phase = PhaseActivating
	}

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
	switch snapshot.Phase {
	case PhaseRequested, PhaseNegotiating, PhaseBackingReady,
		PhaseActivating, PhaseActive:
	default:
		return fmt.Errorf("unknown channel phase %d", snapshot.Phase)
	}
	if snapshot.Source != nil {
		if err := snapshot.Source.Validate(snapshot.Terms); err != nil {
			return err
		}
	}
	if snapshot.Backing != nil {
		if snapshot.Source == nil {
			return fmt.Errorf("backing has no bound VTXO")
		}
		if err := snapshot.Backing.Validate(
			snapshot.Terms, *snapshot.Source,
		); err != nil {
			return err
		}
	}
	if snapshot.Source == nil &&
		(snapshot.Backing != nil || snapshot.ClientFinalized ||
			snapshot.HubFinalized || snapshot.OORFinalized ||
			snapshot.ClientRecoveryReady ||
			snapshot.HubRecoveryReady) {
		return fmt.Errorf("funding facts require a bound VTXO")
	}
	if snapshot.Backing == nil &&
		(snapshot.ClientFinalized || snapshot.HubFinalized) {
		return fmt.Errorf("funding finalization requires signed " +
			"backing")
	}
	requiresBacking := snapshot.Phase == PhaseBackingReady ||
		snapshot.Phase == PhaseActivating ||
		snapshot.Phase == PhaseActive
	if requiresBacking && !backingReady(snapshot) {
		return fmt.Errorf("phase %s requires finalized backing",
			snapshot.Phase)
	}
	if snapshot.OORFinalized && !backingReady(snapshot) {
		return fmt.Errorf("OOR finalization requires finalized backing")
	}
	if (snapshot.ClientRecoveryReady || snapshot.HubRecoveryReady) &&
		!backingReady(snapshot) {
		return fmt.Errorf("channel recovery requires finalized backing")
	}
	switch snapshot.Phase {
	case PhaseRequested:
		if snapshot.Source != nil {
			return fmt.Errorf("requested channel has advanced " +
				"facts")
		}
		if snapshot.Backing != nil || snapshot.ClientFinalized ||
			snapshot.HubFinalized || snapshot.OORFinalized ||
			snapshot.ClientRecoveryReady ||
			snapshot.HubRecoveryReady {
			return fmt.Errorf("requested channel has advanced " +
				"facts")
		}

	case PhaseNegotiating:
		if snapshot.Source == nil {
			return fmt.Errorf("negotiating channel has no bound " +
				"VTXO")
		}
		if snapshot.OORFinalized || snapshot.ClientRecoveryReady ||
			snapshot.HubRecoveryReady {
			return fmt.Errorf("negotiating channel has terminal " +
				"OOR facts")
		}

	case PhaseBackingReady:
		if snapshot.OORFinalized && snapshot.RecoveryReady() {
			return fmt.Errorf("recovery-ready channel must be " +
				"activating")
		}

	case PhaseActivating, PhaseActive:
		if !snapshot.OORFinalized || !snapshot.RecoveryReady() {
			return fmt.Errorf("channel advanced before OOR " +
				"finalization and recovery installation")
		}
	}

	return nil
}

// backingReady checks the durable prerequisites for committing the OOR source.
func backingReady(snapshot Snapshot) bool {
	return snapshot.Source != nil && snapshot.Backing != nil &&
		snapshot.ClientFinalized && snapshot.HubFinalized
}

// bindingsEqual checks idempotency for an immutable VTXO binding.
func bindingsEqual(a, b VTXOBinding) bool {
	return a.OutPoint == b.OutPoint && a.Amount == b.Amount &&
		a.OORSessionID == b.OORSessionID &&
		string(a.PolicyTemplate) == string(b.PolicyTemplate) &&
		string(a.PkScript) == string(b.PkScript)
}

// backingsEqual checks idempotency for immutable transaction bytes.
func backingsEqual(a, b Backing) bool {
	return a.ChannelPoint == b.ChannelPoint &&
		string(a.Transaction) == string(b.Transaction)
}
