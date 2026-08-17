package arkchannel

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
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
		switch event.(type) {
		case *SourceSpent, *RecoveryPackageInstalled, *OORFinalized:
			action, err := PendingAction(next)
			if err != nil {
				return nil, err
			}

			return transitionTo(next, action), nil
		}

		return transitionTo(next, nil), nil
	}

	action, err := advance(&next)
	if err != nil {
		return nil, err
	}
	_, sourceSpent := event.(*SourceSpent)
	_, recoveryInstalled := event.(*RecoveryPackageInstalled)
	_, oorFinalized := event.(*OORFinalized)
	if next.Phase == previousPhase && !sourceSpent &&
		!recoveryInstalled && !oorFinalized {

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
		if snapshot.OORFinalized && !snapshot.RecoveryReady {
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

	case PhaseCancelling:
		if !snapshot.OORAborted {
			return &AbortOOR{
				Terms:  snapshot.Terms.Clone(),
				Source: snapshot.Source.Clone(),
				Reason: snapshot.Failure,
			}, nil
		}

		var backing *Backing
		if snapshot.Backing != nil {
			clone := snapshot.Backing.Clone()
			backing = &clone
		}

		return &CancelFunding{
			Terms:   snapshot.Terms.Clone(),
			Backing: backing,
		}, nil

	case PhaseMaterializing:
		return &PublishChannel{
			Terms:   snapshot.Terms.Clone(),
			Source:  snapshot.Source.Clone(),
			Backing: snapshot.Backing.Clone(),
		}, nil

	case PhaseOnChain:
		if snapshot.SourceConflict != nil {
			return &ForceCloseChannel{
				Backing: snapshot.Backing.Clone(),
			}, nil
		}

	case PhaseCoopClosing:
		return &NegotiateCooperativeClose{
			Terms:   snapshot.Terms.Clone(),
			Source:  snapshot.Source.Clone(),
			Backing: snapshot.Backing.Clone(),
			Request: snapshot.CooperativeCloseRequest.Clone(),
		}, nil

	case PhaseCoopCloseSigned:
		return &PublishCooperativeClose{
			Terms:  snapshot.Terms.Clone(),
			Source: snapshot.Source.Clone(),
			Close:  snapshot.CooperativeClose.Clone(),
		}, nil

	case PhaseCoopClosePublished:
		return &FinalizeCooperativeClose{
			Terms:   snapshot.Terms.Clone(),
			Source:  snapshot.Source.Clone(),
			Backing: snapshot.Backing.Clone(),
			Request: snapshot.CooperativeCloseRequest.Clone(),
			Close:   snapshot.CooperativeClose.Clone(),
		}, nil

	default:
		return nil, nil
	}

	return nil, nil
}

// applyEvent mutates only the fact directly asserted by an event.
func applyEvent(next *Snapshot, event Event) (bool, error) {
	switch event := event.(type) {
	case *BindVTXO:
		return applyBinding(next, event.Binding)

	case *FundingPeerReady:
		return applyFundingPeerReady(next)

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

		if !next.OORAborted {
			return false, fmt.Errorf("cannot finish funding " +
				"cancellation before OOR abort")
		}

		next.Phase = PhaseFailed

		return true, nil

	case *OORFinalized:
		return applyOORFinalized(next, event.SessionID)

	case *RecoveryPackageInstalled:
		return applyRecoveryPackageInstalled(next)

	case *OORAborted:
		return applyOORAborted(
			next, event.SessionID, event.Reason,
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

	case *SourceSpent:
		return applySourceSpent(
			next, event.OutPoint, event.SpendingTxID,
		)

	case *BackingPublished:
		return applyBackingPublished(next, event.TxID)

	case *BackingObserved:
		return applyBackingObserved(next, event.TxID)

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

	case *RequestCooperativeClose:
		return applyCooperativeCloseRequest(next, event.Request)

	case *CooperativeCloseSigned:
		return applyCooperativeCloseSigned(
			next, event.Close, event.Party,
		)

	case *CooperativeClosePublished:
		return applyCooperativeClosePublished(next, event.TxID)

	case *CooperativeCloseFinalized:
		return applyCooperativeCloseFinalized(next, event.Party)

	case *CooperativeCloseAborted:
		return applyCooperativeCloseAborted(next)

	case *Fail:
		return applyFailure(next, event.Reason)

	default:
		return false, fmt.Errorf("unknown channel event %T", event)
	}
}

// applyBinding binds the exact source once. Client-funded promotions can start
// immediately because the protocol binds the hub first. Hub-funded receive
// channels wait for the client's explicit durable readiness barrier.
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
	if next.Terms.Kind == KindPromotion {
		next.Phase = PhaseNegotiating
	}

	return true, nil
}

// applyFundingPeerReady starts a hub-funded receive channel only after its
// peer has independently persisted and validated the exact prepared source.
func applyFundingPeerReady(next *Snapshot) (bool, error) {
	if next.Terms.Kind != KindReceiveIntent {
		return false, fmt.Errorf("funding readiness is only valid " +
			"for receive intents")
	}
	if next.Source == nil {
		return false, fmt.Errorf("cannot accept funding readiness " +
			"before VTXO binding")
	}
	if next.Phase != PhaseRequested {
		return false, nil
	}

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
	if next.OORAborted {
		return false, fmt.Errorf("cannot finalize an aborted OOR " +
			"transfer")
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
func applyRecoveryPackageInstalled(next *Snapshot) (bool, error) {
	if next.Source == nil || next.Backing == nil || !backingReady(*next) {
		return false, fmt.Errorf("cannot install channel recovery " +
			"before finalized backing")
	}
	if next.OORAborted || next.Phase == PhaseCancelling ||
		next.Phase == PhaseFailed {
		return false, fmt.Errorf("cannot install recovery for " +
			"abandoned channel")
	}
	if next.RecoveryReady {
		return false, nil
	}
	next.RecoveryReady = true

	return true, nil
}

// applySourceSpent persists the first ancestor conflict and drives the
// virtual channel toward its already-negotiated on-chain resolution path.
func applySourceSpent(next *Snapshot, outpoint wire.OutPoint,
	spendingTxID chainhash.Hash) (bool, error) {

	if outpoint == (wire.OutPoint{}) || spendingTxID == (chainhash.Hash{}) {
		return false, fmt.Errorf("source spend evidence is incomplete")
	}
	if next.SourceConflict != nil {
		return false, nil
	}
	if !next.RecoveryReady || !next.OORFinalized {
		return false, fmt.Errorf("cannot handle source spend before " +
			"recovery is ready")
	}
	next.SourceConflict = &SourceConflict{
		OutPoint: outpoint, SpendingTxID: spendingTxID,
	}

	switch next.Phase {
	case PhaseActive:
		next.Phase = PhaseMaterializing

	case PhaseCoopClosing, PhaseCoopCloseSigned:
		// Until the OOR transfer itself is finalized, the hub signature
		// is only an authorization and cannot supersede independently
		// observed on-chain ancestry. Discard the pending close and
		// hand the already signed backing transaction to lnd.
		next.CooperativeCloseRequest = nil
		next.CooperativeClose = nil
		next.ClientCloseSigned = false
		next.HubCloseSigned = false
		next.ClientCloseFinalized = false
		next.HubCloseFinalized = false
		next.Phase = PhaseMaterializing

	case PhaseActivating, PhaseMaterializing, PhaseOnChain,
		PhaseCoopClosePublished, PhaseClosed:

	default:
		return false, fmt.Errorf("cannot handle source spend from %s",
			next.Phase)
	}

	return true, nil
}

// applyOORAborted records a definitive pre-PONR failure, allowing lnd's
// pending channel reservation to be removed without risking a gifted VTXO.
func applyOORAborted(next *Snapshot, sessionID [32]byte,
	reason string) (bool, error) {

	if next.Source == nil {
		return false, fmt.Errorf("cannot abort OOR before VTXO binding")
	}
	if next.Source.OORSessionID != sessionID {
		return false, fmt.Errorf("aborted OOR session does not match " +
			"channel source")
	}
	if next.OORFinalized {
		return false, fmt.Errorf("cannot abort a finalized OOR " +
			"transfer")
	}
	if next.OORAborted {
		return false, nil
	}
	if next.Phase != PhaseCancelling && next.Phase != PhaseBackingReady {
		return false, fmt.Errorf("cannot abort OOR from %s", next.Phase)
	}
	if next.Failure == "" {
		if reason == "" {
			return false, fmt.Errorf("OOR abort reason is required")
		}
		next.Failure = reason
	}

	next.OORAborted = true
	next.Phase = PhaseCancelling

	return true, nil
}

// applyChannelActive verifies lnd activated the negotiated funding output.
func applyChannelActive(next *Snapshot,
	channelPoint wire.OutPoint) (bool, error) {

	if next.Phase == PhaseActive || next.Phase == PhaseMaterializing ||
		next.Phase == PhaseOnChain || next.Phase == PhaseCoopClosing ||
		next.Phase == PhaseCoopCloseSigned ||
		next.Phase == PhaseCoopClosePublished ||
		next.Phase == PhaseClosed {

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

	if next.SourceConflict != nil {
		next.Phase = PhaseMaterializing
	} else {
		next.Phase = PhaseActive
	}

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

// applyBackingObserved gives independently observed chain state precedence
// over a virtual or unfinished cooperative lifecycle. A fully resolved lnd
// channel proves any conflicting OOR close can no longer finalize.
func applyBackingObserved(next *Snapshot, txID [32]byte) (bool, error) {
	if next.Backing == nil || next.Backing.ChannelPoint.Hash != txID {
		return false, fmt.Errorf("observed backing transaction does " +
			"not match")
	}
	if next.Phase == PhaseOnChain {
		return false, nil
	}
	switch next.Phase {
	case PhaseActivating, PhaseActive, PhaseMaterializing,
		PhaseCoopClosing, PhaseCoopCloseSigned:

	default:
		return false, fmt.Errorf("cannot observe backing from %s",
			next.Phase)
	}

	next.CooperativeCloseRequest = nil
	next.CooperativeClose = nil
	next.ClientCloseSigned = false
	next.HubCloseSigned = false
	next.ClientCloseFinalized = false
	next.HubCloseFinalized = false
	next.BackingPublished = true
	next.Phase = PhaseOnChain

	return true, nil
}

// applyCooperativeCloseRequest fixes all payout terms before either endpoint
// quiesces its native lnd link.
func applyCooperativeCloseRequest(next *Snapshot,
	request CooperativeCloseRequest) (bool, error) {

	if err := request.Validate(); err != nil {
		return false, err
	}
	if next.CooperativeCloseRequest != nil {
		if cooperativeCloseRequestsEqual(
			*next.CooperativeCloseRequest, request,
		) {
			return false, nil
		}

		return false, fmt.Errorf("channel already has another " +
			"cooperative close request")
	}
	if next.Phase != PhaseActive {
		return false, fmt.Errorf("cannot cooperatively close "+
			"channel from %s", next.Phase)
	}
	clone := request.Clone()
	next.CooperativeCloseRequest = &clone
	next.Phase = PhaseCoopClosing

	return true, nil
}

// applyCooperativeCloseSigned stores the hub-authorized OOR close and one
// endpoint's durable acknowledgement. OOR submission becomes replayable only
// after both endpoint databases acknowledge the same artifact.
func applyCooperativeCloseSigned(next *Snapshot, settlement CooperativeClose,
	party Party) (bool, error) {

	if next.CooperativeCloseRequest == nil || next.Source == nil {
		return false, fmt.Errorf("cooperative close request is missing")
	}
	if party != PartyClient && party != PartyHub {
		return false, fmt.Errorf("unknown cooperative close party %d",
			party)
	}
	if err := settlement.Validate(
		next.Terms, *next.Source, *next.CooperativeCloseRequest,
	); err != nil {
		return false, err
	}
	if next.Phase != PhaseCoopClosing &&
		next.Phase != PhaseCoopCloseSigned &&
		next.Phase != PhaseCoopClosePublished &&
		next.Phase != PhaseClosed {
		return false, fmt.Errorf("cannot authorize cooperative "+
			"close from %s", next.Phase)
	}
	changed := false
	if next.CooperativeClose != nil {
		if !cooperativeClosesEqual(
			*next.CooperativeClose, settlement,
		) {
			return false, fmt.Errorf("channel already has " +
				"another cooperative close authorization")
		}
	} else {
		clone := settlement.Clone()
		next.CooperativeClose = &clone
		changed = true
	}
	switch party {
	case PartyClient:
		if !next.ClientCloseSigned {
			next.ClientCloseSigned = true
			changed = true
		}

	case PartyHub:
		if !next.HubCloseSigned {
			next.HubCloseSigned = true
			changed = true
		}
	}
	if next.ClientCloseSigned && next.HubCloseSigned &&
		next.Phase == PhaseCoopClosing {

		next.Phase = PhaseCoopCloseSigned
		changed = true
	}

	return changed, nil
}

// applyCooperativeClosePublished verifies the ordinary OOR actor finalized the
// exact close before lnd state can be archived. The historical function name
// is retained because its event type predates the OOR implementation.
func applyCooperativeClosePublished(next *Snapshot,
	txID chainhash.Hash) (bool, error) {

	if next.CooperativeClose == nil {
		return false, fmt.Errorf("authorized cooperative close is " +
			"missing")
	}
	if next.CooperativeClose.TxID != txID {
		return false, fmt.Errorf("finalized cooperative close does " +
			"not match")
	}
	if next.Phase == PhaseCoopClosePublished || next.Phase == PhaseClosed {
		return false, nil
	}
	if next.Phase != PhaseCoopCloseSigned {
		return false, fmt.Errorf("cannot finalize cooperative "+
			"close from %s", next.Phase)
	}
	next.Phase = PhaseCoopClosePublished

	return true, nil
}

// applyCooperativeCloseFinalized records one lnd archival acknowledgement and
// closes the Ark FSM only after both endpoint databases are complete.
func applyCooperativeCloseFinalized(next *Snapshot, party Party) (bool, error) {
	if next.CooperativeClose == nil ||
		next.Phase != PhaseCoopClosePublished &&
			next.Phase != PhaseClosed {
		return false, fmt.Errorf("cannot finalize cooperative "+
			"close from %s", next.Phase)
	}
	changed := false
	switch party {
	case PartyClient:
		if !next.ClientCloseFinalized {
			next.ClientCloseFinalized = true
			changed = true
		}

	case PartyHub:
		if !next.HubCloseFinalized {
			next.HubCloseFinalized = true
			changed = true
		}

	default:
		return false, fmt.Errorf("unknown cooperative close party %d",
			party)
	}
	if !changed {
		return false, nil
	}
	if next.ClientCloseFinalized && next.HubCloseFinalized {
		next.Phase = PhaseClosed
	}

	return true, nil
}

// applyCooperativeCloseAborted permits recovery only before the hub-authorized
// OOR package is durable at both endpoints.
func applyCooperativeCloseAborted(next *Snapshot) (bool, error) {
	if next.Phase == PhaseActive && next.CooperativeCloseRequest == nil {
		return false, nil
	}
	if next.Phase != PhaseCoopClosing || next.CooperativeClose != nil {
		return false, fmt.Errorf("cannot abort cooperative "+
			"close from %s", next.Phase)
	}
	next.CooperativeCloseRequest = nil
	next.Phase = PhaseActive

	return true, nil
}

// applyFailure permits abandonment only before the OOR commit action is
// durable. A commit-time failure must arrive as OORAborted so the coordinator
// knows the transfer stayed before its point of no return.
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
	if next.Phase != PhaseRequested && next.Phase != PhaseNegotiating {
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

	case PhaseActivating:
		return &ActivateChannel{
			Terms:   next.Terms.Clone(),
			Backing: next.Backing.Clone(),
		}, nil

	case PhaseBackingReady:
		if next.OORFinalized && next.RecoveryReady {
			next.Phase = PhaseActivating

			return &ActivateChannel{
				Terms:   next.Terms.Clone(),
				Backing: next.Backing.Clone(),
			}, nil
		}
		if next.OORFinalized {
			return &PrepareRecovery{
				Terms:  next.Terms.Clone(),
				Source: next.Source.Clone(),
			}, nil
		}

		return &CommitOOR{
			Terms:  next.Terms.Clone(),
			Source: next.Source.Clone(),
		}, nil

	case PhaseMaterializing:
		return &PublishChannel{
			Terms:   next.Terms.Clone(),
			Source:  next.Source.Clone(),
			Backing: next.Backing.Clone(),
		}, nil

	case PhaseOnChain:
		if next.SourceConflict != nil {
			return &ForceCloseChannel{
				Backing: next.Backing.Clone(),
			}, nil
		}

	case PhaseCoopClosing:
		return &NegotiateCooperativeClose{
			Terms:   next.Terms.Clone(),
			Source:  next.Source.Clone(),
			Backing: next.Backing.Clone(),
			Request: next.CooperativeCloseRequest.Clone(),
		}, nil

	case PhaseCoopCloseSigned:
		return &PublishCooperativeClose{
			Terms:  next.Terms.Clone(),
			Source: next.Source.Clone(),
			Close:  next.CooperativeClose.Clone(),
		}, nil

	case PhaseCoopClosePublished:
		if next.ClientCloseFinalized && next.HubCloseFinalized {
			next.Phase = PhaseClosed

			return nil, nil
		}

		return &FinalizeCooperativeClose{
			Terms:   next.Terms.Clone(),
			Source:  next.Source.Clone(),
			Backing: next.Backing.Clone(),
			Request: next.CooperativeCloseRequest.Clone(),
			Close:   next.CooperativeClose.Clone(),
		}, nil

	case PhaseCancelling:
		if !next.OORAborted {
			return &AbortOOR{
				Terms:  next.Terms.Clone(),
				Source: next.Source.Clone(),
				Reason: next.Failure,
			}, nil
		}

		var backing *Backing
		if next.Backing != nil {
			clone := next.Backing.Clone()
			backing = &clone
		}

		return &CancelFunding{
			Terms:   next.Terms.Clone(),
			Backing: backing,
		}, nil

	case PhaseRequested, PhaseActive, PhaseClosed, PhaseFailed:
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
	if snapshot.Phase < PhaseRequested ||
		snapshot.Phase > PhaseCoopClosePublished {
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
		snapshot.ClientFinalized || snapshot.HubFinalized ||
		snapshot.OORFinalized || snapshot.OORAborted ||
		snapshot.RecoveryReady || snapshot.SourceConflict != nil) {
		return fmt.Errorf("funding facts require a bound VTXO")
	}
	if snapshot.Backing == nil && (snapshot.ClientFinalized ||
		snapshot.HubFinalized) {
		return fmt.Errorf("funding finalization requires signed " +
			"backing")
	}
	requiresBacking := snapshot.Phase == PhaseBackingReady ||
		snapshot.Phase == PhaseActivating ||
		snapshot.Phase == PhaseActive ||
		snapshot.Phase == PhaseMaterializing ||
		snapshot.Phase == PhaseOnChain ||
		snapshot.Phase == PhaseCoopClosing ||
		snapshot.Phase == PhaseCoopCloseSigned ||
		snapshot.Phase == PhaseCoopClosePublished ||
		snapshot.Phase == PhaseClosed
	if requiresBacking && !backingReady(snapshot) {
		return fmt.Errorf("phase %s requires finalized backing",
			snapshot.Phase)
	}
	if snapshot.OORFinalized && !backingReady(snapshot) {
		return fmt.Errorf("OOR finalization requires finalized backing")
	}
	if snapshot.OORFinalized && snapshot.OORAborted {
		return fmt.Errorf("OOR transfer cannot be finalized and " +
			"aborted")
	}
	if snapshot.RecoveryReady && !backingReady(snapshot) {
		return fmt.Errorf("channel recovery requires finalized backing")
	}
	if snapshot.SourceConflict != nil && (!snapshot.RecoveryReady ||
		!snapshot.OORFinalized) {
		return fmt.Errorf("source conflict requires ready recovery")
	}
	if snapshot.BackingPublished && snapshot.Phase != PhaseOnChain &&
		snapshot.Phase != PhaseClosed {
		return fmt.Errorf("published backing requires on-chain phase")
	}
	if err := validateCooperativeCloseSnapshot(snapshot); err != nil {
		return err
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
		if snapshot.Source != nil &&
			snapshot.Terms.Kind != KindReceiveIntent {
			return fmt.Errorf("only a receive intent may wait " +
				"with a bound VTXO")
		}
		if snapshot.Backing != nil || snapshot.ClientFinalized ||
			snapshot.HubFinalized || snapshot.OORFinalized ||
			snapshot.OORAborted || snapshot.RecoveryReady ||
			snapshot.SourceConflict != nil ||
			snapshot.BackingPublished {
			return fmt.Errorf("requested channel has advanced " +
				"facts")
		}

	case PhaseNegotiating:
		if snapshot.Source == nil {
			return fmt.Errorf("negotiating channel has no bound " +
				"VTXO")
		}
		if snapshot.OORFinalized || snapshot.OORAborted ||
			snapshot.RecoveryReady ||
			snapshot.SourceConflict != nil {
			return fmt.Errorf("negotiating channel has terminal " +
				"OOR facts")
		}

	case PhaseBackingReady:
		if snapshot.OORAborted || snapshot.SourceConflict != nil {
			return fmt.Errorf("backing-ready channel has " +
				"terminal OOR facts")
		}

	case PhaseActivating, PhaseActive, PhaseMaterializing, PhaseOnChain,
		PhaseClosed, PhaseCoopClosing, PhaseCoopCloseSigned,
		PhaseCoopClosePublished:

		if !snapshot.OORFinalized || snapshot.OORAborted ||
			!snapshot.RecoveryReady {
			return fmt.Errorf("channel advanced before OOR " +
				"finalization and recovery installation")
		}

	case PhaseCancelling:
		if snapshot.Source == nil {
			return fmt.Errorf("cancelling channel has no bound " +
				"VTXO")
		}
		if snapshot.OORFinalized || snapshot.BackingPublished {
			return fmt.Errorf("failed channel crossed the safety " +
				"boundary")
		}

	case PhaseFailed:
		if snapshot.OORFinalized || snapshot.BackingPublished {
			return fmt.Errorf("failed channel crossed the safety " +
				"boundary")
		}
		if snapshot.Source != nil && !snapshot.OORAborted {
			return fmt.Errorf("failed channel did not abort " +
				"prepared OOR")
		}
	}

	if snapshot.Phase == PhaseOnChain && !snapshot.BackingPublished {
		return fmt.Errorf("on-chain phase requires published backing")
	}

	return nil
}

// validateCooperativeCloseSnapshot checks artifacts and acknowledgements owned
// by the in-Ark OOR close lifecycle.
func validateCooperativeCloseSnapshot(snapshot Snapshot) error {
	if snapshot.CooperativeCloseRequest != nil {
		request := snapshot.CooperativeCloseRequest
		if err := request.Validate(); err != nil {
			return err
		}
	}
	if snapshot.CooperativeClose != nil {
		if snapshot.Source == nil ||
			snapshot.CooperativeCloseRequest == nil {
			return fmt.Errorf("cooperative close has no request " +
				"or source")
		}
		if err := snapshot.CooperativeClose.Validate(
			snapshot.Terms, *snapshot.Source,
			*snapshot.CooperativeCloseRequest,
		); err != nil {
			return err
		}
	}
	if (snapshot.ClientCloseSigned || snapshot.HubCloseSigned ||
		snapshot.ClientCloseFinalized || snapshot.HubCloseFinalized) &&
		snapshot.CooperativeClose == nil {
		return fmt.Errorf("close acknowledgements require signed " +
			"settlement")
	}

	switch snapshot.Phase {
	case PhaseCoopClosing:
		if snapshot.CooperativeCloseRequest == nil ||
			snapshot.ClientCloseFinalized ||
			snapshot.HubCloseFinalized {
			return fmt.Errorf("cooperative closing phase has " +
				"invalid facts")
		}
		if snapshot.CooperativeClose == nil &&
			(snapshot.ClientCloseSigned ||
				snapshot.HubCloseSigned) {
			return fmt.Errorf("cooperative close acknowledgement " +
				"has no artifact")
		}
		if snapshot.CooperativeClose != nil &&
			(!snapshot.ClientCloseSigned &&
				!snapshot.HubCloseSigned ||
				snapshot.ClientCloseSigned &&
					snapshot.HubCloseSigned) {
			return fmt.Errorf("cooperative close staging has " +
				"invalid acknowledgements")
		}

	case PhaseCoopCloseSigned:
		if snapshot.CooperativeCloseRequest == nil ||
			snapshot.CooperativeClose == nil ||
			!snapshot.ClientCloseSigned ||
			!snapshot.HubCloseSigned ||
			snapshot.ClientCloseFinalized ||
			snapshot.HubCloseFinalized {
			return fmt.Errorf("authorized cooperative close has " +
				"invalid facts")
		}

	case PhaseCoopClosePublished:
		if snapshot.CooperativeClose == nil ||
			!snapshot.ClientCloseSigned ||
			!snapshot.HubCloseSigned {
			return fmt.Errorf("finalized cooperative close is " +
				"missing")
		}

	case PhaseClosed:
		if snapshot.CooperativeClose != nil &&
			(!snapshot.ClientCloseSigned ||
				!snapshot.HubCloseSigned ||
				!snapshot.ClientCloseFinalized ||
				!snapshot.HubCloseFinalized) {
			return fmt.Errorf("cooperative close lacks endpoint " +
				"finalization")
		}

	default:
		if snapshot.CooperativeCloseRequest != nil ||
			snapshot.CooperativeClose != nil ||
			snapshot.ClientCloseSigned ||
			snapshot.HubCloseSigned ||
			snapshot.ClientCloseFinalized ||
			snapshot.HubCloseFinalized {
			return fmt.Errorf("phase %s has cooperative "+
				"close facts", snapshot.Phase)
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

// cooperativeCloseRequestsEqual checks immutable payout instructions.
func cooperativeCloseRequestsEqual(a, b CooperativeCloseRequest) bool {
	return a.Initiator == b.Initiator &&
		string(a.ClientDeliveryScript) ==
			string(b.ClientDeliveryScript) &&
		string(a.HubDeliveryScript) == string(b.HubDeliveryScript)
}

// cooperativeClosesEqual checks the hub-authorized OOR close artifact.
func cooperativeClosesEqual(a, b CooperativeClose) bool {
	return a.TxID == b.TxID &&
		cooperativeCloseProposalsEqual(a.Proposal, b.Proposal)
}

// cooperativeCloseProposalsEqual checks canonical unsigned OOR facts.
func cooperativeCloseProposalsEqual(a, b CooperativeCloseProposal) bool {
	return a.CommitmentHeight == b.CommitmentHeight &&
		a.ClientBalance == b.ClientBalance &&
		a.HubBalance == b.HubBalance &&
		a.ClientOutput == b.ClientOutput &&
		a.HubOutput == b.HubOutput &&
		string(a.Transaction) == string(b.Transaction)
}
