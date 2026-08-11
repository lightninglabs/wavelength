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
		return transitionTo(next, nil), nil
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

	switch snapshot.Phase {
	case PhaseNegotiating:
		return &NegotiateFunding{
			Terms:  snapshot.Terms.Clone(),
			Source: snapshot.Source.Clone(),
		}, nil

	case PhaseActivating:
		return &ActivateChannel{
			Terms:   snapshot.Terms.Clone(),
			Backing: snapshot.Backing.Clone(),
		}, nil

	case PhaseCancelling:
		return &CancelFunding{
			Terms: snapshot.Terms.Clone(),
		}, nil

	case PhaseMaterializing:
		return &PublishChannel{
			Terms:   snapshot.Terms.Clone(),
			Source:  snapshot.Source.Clone(),
			Backing: snapshot.Backing.Clone(),
		}, nil

	default:
		return nil, nil
	}
}

// applyEvent mutates only the fact directly asserted by an event.
func applyEvent(next *Snapshot, event Event) (bool, error) {
	switch event := event.(type) {
	case *BindVTXO:
		return applyBinding(next, event.Binding)

	case *FundingFinalized:
		return applyFundingFinalized(next, event.Party)

	case *BackingSigned:
		return applyBacking(next, event.Backing)

	case *FundingCanceled:
		if next.Phase == PhaseFailed {
			return false, nil
		}
		if next.Phase != PhaseCancelling {
			return false, fmt.Errorf("cannot finish funding "+
				"cancellation from %s", next.Phase)
		}

		next.Phase = PhaseFailed

		return true, nil

	case *RoundCommitted:
		return applyRoundCommitted(
			next, event.RoundID, event.CommitmentTxID,
		)

	case *RoundConfirmed:
		return applyRoundConfirmed(
			next, event.RoundID, event.CommitmentTxID,
		)

	case *ChannelActive:
		return applyChannelActive(next, wire.OutPoint{
			Hash:  event.ChannelPointHash,
			Index: event.ChannelPointIndex,
		})

	case *Materialize:
		if next.Phase == PhaseMaterializing ||
			next.Phase == PhaseOnChain {
			return false, nil
		}
		if next.Phase != PhaseActive {
			return false, fmt.Errorf("cannot materialize "+
				"channel from %s", next.Phase)
		}

		next.Phase = PhaseMaterializing

		return true, nil

	case *BackingPublished:
		return applyBackingPublished(next, event.TxID)

	case *ChannelClosed:
		if next.Phase == PhaseClosed {
			return false, nil
		}
		if next.Phase != PhaseActive && next.Phase != PhaseOnChain {
			return false, fmt.Errorf("cannot close channel from %s",
				next.Phase)
		}

		next.Phase = PhaseClosed

		return true, nil

	case *Fail:
		return applyFailure(next, event.Reason)

	default:
		return false, fmt.Errorf("unknown channel event %T", event)
	}
}

// applyBinding binds the exact source once and starts lnd negotiation.
func applyBinding(next *Snapshot, binding VTXOBinding) (bool, error) {
	if err := binding.Validate(next.Terms); err != nil {
		return false, err
	}
	if next.Source != nil {
		if bindingsEqual(*next.Source, binding) {
			return false, nil
		}

		return false, fmt.Errorf("channel already bound to another " +
			"VTXO")
	}
	if next.Phase != PhaseRequested {
		return false, fmt.Errorf("cannot bind VTXO from %s", next.Phase)
	}

	clone := binding.Clone()
	next.Source = &clone
	next.Phase = PhaseNegotiating

	return true, nil
}

// applyFundingFinalized records one native lnd finalization acknowledgement.
func applyFundingFinalized(next *Snapshot, party Party) (bool, error) {
	if next.Source == nil {
		return false, fmt.Errorf("cannot finalize funding before " +
			"VTXO binding")
	}
	if next.Phase == PhaseCancelling || next.Phase == PhaseFailed {
		return false, fmt.Errorf("cannot finalize abandoned channel")
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
	if next.Phase == PhaseCancelling || next.Phase == PhaseFailed {
		return false, fmt.Errorf("cannot record backing for " +
			"abandoned channel")
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

// applyRoundCommitted records nonce release only after readiness is durable.
func applyRoundCommitted(next *Snapshot, roundID string,
	commitmentTxID [32]byte) (bool, error) {

	if next.Terms.Kind != KindReceiveIntent {
		return false, fmt.Errorf("promotion has no funding round")
	}
	if !backingReady(*next) {
		return false, fmt.Errorf("cannot commit round before backing " +
			"is ready")
	}
	if err := matchRound(
		*next.Source, roundID, commitmentTxID,
	); err != nil {
		return false, err
	}
	if next.RoundCommitted {
		return false, nil
	}
	if next.Phase != PhaseBackingReady {
		return false, fmt.Errorf("cannot commit round from %s",
			next.Phase)
	}

	next.RoundCommitted = true

	return true, nil
}

// applyRoundConfirmed records confirmation of the exact bound round.
func applyRoundConfirmed(next *Snapshot, roundID string,
	commitmentTxID [32]byte) (bool, error) {

	if next.Terms.Kind != KindReceiveIntent {
		return false, fmt.Errorf("promotion has no funding round")
	}
	if !next.RoundCommitted {
		return false, fmt.Errorf("cannot confirm round before " +
			"commitment")
	}
	if err := matchRound(
		*next.Source, roundID, commitmentTxID,
	); err != nil {
		return false, err
	}
	if next.RoundConfirmed {
		return false, nil
	}

	next.RoundConfirmed = true

	return true, nil
}

// applyChannelActive verifies lnd activated the negotiated funding output.
func applyChannelActive(next *Snapshot,
	channelPoint wire.OutPoint) (bool, error) {

	if next.Phase == PhaseActive || next.Phase == PhaseMaterializing ||
		next.Phase == PhaseOnChain || next.Phase == PhaseClosed {

		if next.Backing.ChannelPoint == channelPoint {
			return false, nil
		}
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

// applyBackingPublished verifies materialization of the expected transaction.
func applyBackingPublished(next *Snapshot, txID [32]byte) (bool, error) {
	if next.Phase == PhaseOnChain {
		if next.Backing.ChannelPoint.Hash == txID {
			return false, nil
		}
	}
	if next.Phase != PhaseMaterializing {
		return false, fmt.Errorf("cannot publish backing from %s",
			next.Phase)
	}
	if next.Backing.ChannelPoint.Hash != txID {
		return false, fmt.Errorf("published backing transaction does " +
			"not match")
	}

	next.BackingPublished = true
	next.Phase = PhaseOnChain

	return true, nil
}

// applyFailure preserves the no-failure boundary once round signing begins.
func applyFailure(next *Snapshot, reason string) (bool, error) {
	if next.Phase == PhaseFailed {
		if next.Failure == reason {
			return false, nil
		}

		return false, fmt.Errorf("channel already failed for another " +
			"reason")
	}
	if next.Phase == PhaseCancelling {
		if next.Failure == reason {
			return false, nil
		}

		return false, fmt.Errorf("channel already cancelling for " +
			"another reason")
	}
	canFailReceiveBacking := next.Phase == PhaseBackingReady &&
		next.Terms.Kind == KindReceiveIntent && !next.RoundCommitted
	if next.Phase != PhaseRequested && next.Phase != PhaseNegotiating &&
		!canFailReceiveBacking {
		return false, fmt.Errorf("cannot fail channel after safety " +
			"boundary")
	}
	if reason == "" {
		return false, fmt.Errorf("failure reason is required")
	}

	next.Failure = reason
	if next.Source == nil {
		next.Phase = PhaseFailed
	} else {
		next.Phase = PhaseCancelling
	}

	return true, nil
}

// advance derives the next phase and replayable action from durable facts.
func advance(next *Snapshot) (Action, error) {
	if next.Phase == PhaseNegotiating && backingReady(*next) {
		next.Phase = PhaseBackingReady
	}

	switch next.Phase {
	case PhaseNegotiating:
		return &NegotiateFunding{
			Terms:  next.Terms.Clone(),
			Source: next.Source.Clone(),
		}, nil

	case PhaseBackingReady:
		if next.Terms.Kind == KindPromotion {
			next.Phase = PhaseActivating

			return &ActivateChannel{
				Terms:   next.Terms.Clone(),
				Backing: next.Backing.Clone(),
			}, nil
		}
		if next.RoundCommitted {
			next.Phase = PhaseAwaitingConfirmation
		}

	case PhaseAwaitingConfirmation:
		if next.RoundConfirmed {
			next.Phase = PhaseActivating

			return &ActivateChannel{
				Terms:   next.Terms.Clone(),
				Backing: next.Backing.Clone(),
			}, nil
		}

	case PhaseMaterializing:
		return &PublishChannel{
			Terms:   next.Terms.Clone(),
			Source:  next.Source.Clone(),
			Backing: next.Backing.Clone(),
		}, nil

	case PhaseCancelling:
		return &CancelFunding{
			Terms: next.Terms.Clone(),
		}, nil

	case PhaseRequested, PhaseActivating, PhaseActive, PhaseOnChain,
		PhaseClosed, PhaseFailed:
	}

	return nil, nil
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
	if snapshot.Phase < PhaseRequested || snapshot.Phase > PhaseFailed {
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
	if snapshot.Source == nil && (snapshot.Backing != nil ||
		snapshot.ClientFinalized || snapshot.HubFinalized) {
		return fmt.Errorf("funding facts require a bound VTXO")
	}
	if snapshot.Phase >= PhaseBackingReady &&
		snapshot.Phase != PhaseCancelling &&
		snapshot.Phase != PhaseFailed && !backingReady(snapshot) {
		return fmt.Errorf("phase %s requires finalized backing",
			snapshot.Phase)
	}
	if snapshot.RoundCommitted &&
		snapshot.Terms.Kind != KindReceiveIntent {
		return fmt.Errorf("promotion cannot have a committed round")
	}
	if snapshot.RoundConfirmed && !snapshot.RoundCommitted {
		return fmt.Errorf("round confirmation requires commitment")
	}
	if snapshot.RoundCommitted && !backingReady(snapshot) {
		return fmt.Errorf("round commitment requires finalized backing")
	}
	if snapshot.BackingPublished && snapshot.Phase != PhaseOnChain &&
		snapshot.Phase != PhaseClosed {
		return fmt.Errorf("published backing requires on-chain phase")
	}
	if (snapshot.Phase == PhaseCancelling ||
		snapshot.Phase == PhaseFailed) && snapshot.Failure == "" {
		return fmt.Errorf("abandoned channel requires a reason")
	}
	if snapshot.Phase != PhaseCancelling &&
		snapshot.Phase != PhaseFailed && snapshot.Failure != "" {
		return fmt.Errorf("non-failed channel cannot have a failure " +
			"reason")
	}

	switch snapshot.Phase {
	case PhaseRequested:
		if snapshot.Source != nil || snapshot.RoundCommitted ||
			snapshot.RoundConfirmed || snapshot.BackingPublished {
			return fmt.Errorf("requested channel has advanced " +
				"facts")
		}

	case PhaseNegotiating:
		if snapshot.Source == nil {
			return fmt.Errorf("negotiating channel has no bound " +
				"VTXO")
		}

	case PhaseBackingReady:
		if snapshot.Terms.Kind != KindReceiveIntent ||
			snapshot.RoundCommitted {
			return fmt.Errorf("backing-ready phase requires an " +
				"uncommitted receive intent")
		}

	case PhaseAwaitingConfirmation:
		if snapshot.Terms.Kind != KindReceiveIntent ||
			!snapshot.RoundCommitted || snapshot.RoundConfirmed {
			return fmt.Errorf("awaiting-confirmation phase has " +
				"invalid round facts")
		}

	case PhaseActivating, PhaseActive, PhaseMaterializing,
		PhaseOnChain, PhaseClosed:

		if snapshot.Terms.Kind == KindReceiveIntent &&
			!snapshot.RoundConfirmed {
			return fmt.Errorf("receive channel advanced before " +
				"round confirmation")
		}

	case PhaseCancelling:
		if snapshot.Source == nil {
			return fmt.Errorf("cancelling channel has no bound " +
				"VTXO")
		}
		if snapshot.RoundCommitted || snapshot.RoundConfirmed ||
			snapshot.BackingPublished {
			return fmt.Errorf("failed channel crossed the safety " +
				"boundary")
		}

	case PhaseFailed:
		if snapshot.RoundCommitted || snapshot.RoundConfirmed ||
			snapshot.BackingPublished {
			return fmt.Errorf("failed channel crossed the safety " +
				"boundary")
		}
	}
	if snapshot.Phase == PhaseOnChain && !snapshot.BackingPublished {
		return fmt.Errorf("on-chain phase requires published backing")
	}

	return nil
}

// backingReady checks the three durable prerequisites for round signing.
func backingReady(snapshot Snapshot) bool {
	return snapshot.Source != nil && snapshot.Backing != nil &&
		snapshot.ClientFinalized && snapshot.HubFinalized
}

// matchRound correlates confirmation events with the exact bound output.
func matchRound(source VTXOBinding, roundID string,
	commitmentTxID [32]byte) error {

	if source.RoundID != roundID {
		return fmt.Errorf("round ID %s does not match bound round %s",
			roundID, source.RoundID)
	}
	if source.CommitmentTxID != commitmentTxID {
		return fmt.Errorf("commitment transaction does not match " +
			"bound round")
	}

	return nil
}

// bindingsEqual checks idempotency for an immutable VTXO binding.
func bindingsEqual(a, b VTXOBinding) bool {
	return a.OutPoint == b.OutPoint && a.Amount == b.Amount &&
		a.RoundID == b.RoundID &&
		a.CommitmentTxID == b.CommitmentTxID &&
		string(a.PolicyTemplate) == string(b.PolicyTemplate) &&
		string(a.PkScript) == string(b.PkScript)
}

// backingsEqual checks idempotency for immutable transaction bytes.
func backingsEqual(a, b Backing) bool {
	return a.ChannelPoint == b.ChannelPoint &&
		string(a.Transaction) == string(b.Transaction)
}
