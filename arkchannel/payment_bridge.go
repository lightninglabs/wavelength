package arkchannel

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
)

// PaymentDirection identifies which side of the private channel holds value
// before the bridge dispatches the same payment hash on its other leg.
type PaymentDirection uint8

const (
	// PaymentOutgoing moves a held private-channel HTLC to public
	// Lightning.
	PaymentOutgoing PaymentDirection = iota + 1

	// PaymentIncoming moves a held public Lightning HTLC to the private
	// channel.
	PaymentIncoming
)

// String returns the durable payment direction name.
func (d PaymentDirection) String() string {
	switch d {
	case PaymentOutgoing:
		return "outgoing"

	case PaymentIncoming:
		return "incoming"

	default:
		return "unknown"
	}
}

// PaymentPhase is the small cross-system lifecycle around lnd's authoritative
// invoice and payment state machines.
type PaymentPhase uint8

const (
	// PaymentRegistered means immutable bridge terms are durable.
	PaymentRegistered PaymentPhase = iota + 1

	// PaymentSourceReady means the outgoing private hold invoice exists.
	PaymentSourceReady

	// PaymentSourceLocked means the source HTLC is irrevocably identified
	// and held while the destination is dispatched.
	PaymentSourceLocked

	// PaymentDestinationInFlight means lnd owns a durable destination
	// payment attempt for the same payment hash.
	PaymentDestinationInFlight

	// PaymentPreimageKnown means the destination settled and the exact
	// preimage is durable before the source is released.
	PaymentPreimageKnown

	// PaymentCompleted means the source was released with that preimage.
	PaymentCompleted

	// PaymentSourceFailing means the destination failed before revealing a
	// preimage and the held source must be failed.
	PaymentSourceFailing

	// PaymentFailed means both legs ended without moving value.
	PaymentFailed

	// PaymentVHTLCFallback hands an incoming payment to the existing vHTLC
	// receive lifecycle because no private inbound capacity was available.
	PaymentVHTLCFallback

	// PaymentNeedsIntervention means a known preimage could not be applied
	// automatically and value safety requires operator attention.
	PaymentNeedsIntervention
)

// String returns the durable payment phase name.
func (p PaymentPhase) String() string {
	switch p {
	case PaymentRegistered:
		return "registered"

	case PaymentSourceReady:
		return "source_ready"

	case PaymentSourceLocked:
		return "source_locked"

	case PaymentDestinationInFlight:
		return "destination_in_flight"

	case PaymentPreimageKnown:
		return "preimage_known"

	case PaymentCompleted:
		return "completed"

	case PaymentSourceFailing:
		return "source_failing"

	case PaymentFailed:
		return "failed"

	case PaymentVHTLCFallback:
		return "vhtlc_fallback"

	case PaymentNeedsIntervention:
		return "needs_intervention"

	default:
		return "unknown"
	}
}

// IsTerminal reports whether the direct bridge has no pending side effect.
func (p PaymentPhase) IsTerminal() bool {
	return p == PaymentCompleted || p == PaymentFailed ||
		p == PaymentVHTLCFallback || p == PaymentNeedsIntervention
}

// PaymentCircuit identifies the public HTLC held by the operator interceptor.
type PaymentCircuit struct {
	IncomingChannelID uint64
	IncomingHTLCID    uint64
	OutgoingSCID      uint64
	IncomingExpiry    uint32
}

// PaymentBridgeSnapshot is the durable boundary between the private lnd
// runtime and the operator's public Lightning node.
type PaymentBridgeSnapshot struct {
	Direction         PaymentDirection
	Phase             PaymentPhase
	PaymentHash       lntypes.Hash
	ClientNodeKey     [33]byte
	ChannelID         ID
	ReservedSCID      uint64
	DestinationSCID   uint64
	SourceAmount      btcutil.Amount
	DestinationAmount btcutil.Amount
	ServerFee         btcutil.Amount
	RoutingFeeBudget  btcutil.Amount
	PublicInvoice     string
	Circuit           *PaymentCircuit
	Preimage          *lntypes.Preimage
	Failure           string
}

// Clone returns a snapshot without pointer aliases.
func (s PaymentBridgeSnapshot) Clone() PaymentBridgeSnapshot {
	if s.Circuit != nil {
		circuit := *s.Circuit
		s.Circuit = &circuit
	}
	if s.Preimage != nil {
		preimage := *s.Preimage
		s.Preimage = &preimage
	}

	return s
}

// Validate checks the same-hash and value-conservation bridge invariants.
func (s PaymentBridgeSnapshot) Validate() error {
	if s.PaymentHash == (lntypes.Hash{}) {
		return fmt.Errorf("payment hash is required")
	}
	if err := validateNodeKey("client", s.ClientNodeKey); err != nil {
		return err
	}
	if s.SourceAmount <= 0 || s.DestinationAmount <= 0 {
		return fmt.Errorf("bridge amounts must be positive")
	}
	if s.SourceAmount < s.DestinationAmount {
		return fmt.Errorf("source amount cannot be below destination " +
			"amount")
	}
	if s.Phase < PaymentRegistered || s.Phase > PaymentNeedsIntervention {
		return fmt.Errorf("unknown payment phase %d", s.Phase)
	}

	switch s.Direction {
	case PaymentOutgoing:
		if s.PublicInvoice == "" {
			return fmt.Errorf("outgoing bridge requires public " +
				"invoice")
		}
		if s.ChannelID == (ID{}) || s.ReservedSCID == 0 {
			return fmt.Errorf("outgoing bridge requires active " +
				"channel")
		}
		if s.Circuit != nil {
			return fmt.Errorf("outgoing bridge cannot bind " +
				"public circuit")
		}
		if s.ServerFee < 0 || s.RoutingFeeBudget < 0 ||
			s.ServerFee > btcutil.MaxSatoshi-s.RoutingFeeBudget ||
			s.DestinationAmount > btcutil.MaxSatoshi-
				s.ServerFee-s.RoutingFeeBudget ||
			s.SourceAmount != s.DestinationAmount+s.ServerFee+
				s.RoutingFeeBudget {
			return fmt.Errorf("outgoing bridge fee accounting is " +
				"invalid")
		}

	case PaymentIncoming:
		if s.PublicInvoice != "" {
			return fmt.Errorf("incoming bridge cannot store " +
				"public invoice")
		}
		if s.ReservedSCID == 0 {
			return fmt.Errorf("incoming bridge requires reserved " +
				"SCID")
		}
		if s.ServerFee != 0 || s.RoutingFeeBudget != 0 ||
			s.SourceAmount != s.DestinationAmount {
			return fmt.Errorf("incoming bridge amounts must match")
		}

	default:
		return fmt.Errorf("unknown payment direction %d", s.Direction)
	}

	if s.Circuit != nil && s.Direction != PaymentIncoming {
		return fmt.Errorf("only incoming bridge can bind public " +
			"circuit")
	}
	if s.Preimage != nil && s.Preimage.Hash() != s.PaymentHash {
		return fmt.Errorf("bridge preimage does not match payment hash")
	}
	if (s.Phase == PaymentPreimageKnown ||
		s.Phase == PaymentCompleted ||
		s.Phase == PaymentNeedsIntervention) && s.Preimage == nil {
		return fmt.Errorf("phase %s requires preimage", s.Phase)
	}
	if (s.Phase == PaymentSourceFailing || s.Phase == PaymentFailed ||
		s.Phase == PaymentVHTLCFallback ||
		s.Phase == PaymentNeedsIntervention) && s.Failure == "" {
		return fmt.Errorf("phase %s requires reason", s.Phase)
	}
	if s.Preimage != nil && (s.Phase == PaymentSourceFailing ||
		s.Phase == PaymentFailed || s.Phase == PaymentVHTLCFallback) {
		return fmt.Errorf("known preimage cannot enter failure path")
	}

	return nil
}

// PaymentBridgeEvent is a closed set of durable bridge facts.
type PaymentBridgeEvent interface {
	paymentBridgeEventSealed()
}

// PaymentSourcePrepared records creation of the outgoing hold invoice.
type PaymentSourcePrepared struct{}

func (*PaymentSourcePrepared) paymentBridgeEventSealed() {}

// PaymentSourceUnavailable records that the client could not lock the
// outgoing private HTLC after the operator installed its hold invoice.
type PaymentSourceUnavailable struct {
	Reason string
}

func (*PaymentSourceUnavailable) paymentBridgeEventSealed() {}

// PaymentSourceHTLCLocked records the exact held source HTLC.
type PaymentSourceHTLCLocked struct {
	Circuit *PaymentCircuit
}

func (*PaymentSourceHTLCLocked) paymentBridgeEventSealed() {}

// PaymentDestinationStarted records that lnd owns the destination attempt.
type PaymentDestinationStarted struct {
	ChannelID       ID
	DestinationSCID uint64
}

func (*PaymentDestinationStarted) paymentBridgeEventSealed() {}

// PaymentDestinationSettled records the destination's revealed preimage.
type PaymentDestinationSettled struct {
	Preimage lntypes.Preimage
}

func (*PaymentDestinationSettled) paymentBridgeEventSealed() {}

// PaymentSourceSettled records release of the source with the preimage.
type PaymentSourceSettled struct{}

func (*PaymentSourceSettled) paymentBridgeEventSealed() {}

// PaymentDestinationFailed records definitive destination failure.
type PaymentDestinationFailed struct {
	Reason string
}

func (*PaymentDestinationFailed) paymentBridgeEventSealed() {}

// PaymentSourceFailed records definitive cancellation of the held source.
type PaymentSourceFailed struct{}

func (*PaymentSourceFailed) paymentBridgeEventSealed() {}

// PaymentFallbackSelected hands an incoming payment to the vHTLC lifecycle.
type PaymentFallbackSelected struct {
	Reason string
}

func (*PaymentFallbackSelected) paymentBridgeEventSealed() {}

// PaymentInterventionRequired records a failure after a preimage was known.
type PaymentInterventionRequired struct {
	Reason string
}

func (*PaymentInterventionRequired) paymentBridgeEventSealed() {}

// PaymentBridgeAction is an idempotent side effect implied by durable state.
type PaymentBridgeAction interface {
	paymentBridgeActionSealed()
}

// PreparePaymentSource creates the outgoing private hold invoice.
type PreparePaymentSource struct{}

func (*PreparePaymentSource) paymentBridgeActionSealed() {}

// WaitPaymentSource waits until the source HTLC is held.
type WaitPaymentSource struct{}

func (*WaitPaymentSource) paymentBridgeActionSealed() {}

// DispatchPaymentDestination sends or resumes the same hash on the other leg.
type DispatchPaymentDestination struct{}

func (*DispatchPaymentDestination) paymentBridgeActionSealed() {}

// SettlePaymentSource releases the source with the durable preimage.
type SettlePaymentSource struct {
	Preimage lntypes.Preimage
}

func (*SettlePaymentSource) paymentBridgeActionSealed() {}

// FailPaymentSource cancels the source before any preimage is known.
type FailPaymentSource struct {
	Reason string
}

func (*FailPaymentSource) paymentBridgeActionSealed() {}

// PaymentBridgeEnvironment names one pure bridge machine.
type PaymentBridgeEnvironment struct {
	PaymentHash lntypes.Hash
}

// Name returns the stable state machine name.
func (e *PaymentBridgeEnvironment) Name() string {
	return fmt.Sprintf("ark_channel_payment_%x", e.PaymentHash[:4])
}

// PaymentBridgeState is one protofsm state around native lnd lifecycles.
type PaymentBridgeState interface {
	protofsm.State[
		PaymentBridgeEvent, PaymentBridgeAction,
		*PaymentBridgeEnvironment,
	]

	Snapshot() PaymentBridgeSnapshot
}

type paymentBridgeState struct {
	snapshot PaymentBridgeSnapshot
}

// NewPaymentBridgeState validates and creates one registered bridge state.
func NewPaymentBridgeState(snapshot PaymentBridgeSnapshot) (PaymentBridgeState,
	error) {

	if snapshot.DestinationSCID != 0 || snapshot.Circuit != nil ||
		snapshot.Preimage != nil || snapshot.Failure != "" {
		return nil, fmt.Errorf("new payment bridge contains " +
			"lifecycle state")
	}
	if snapshot.Direction == PaymentIncoming &&
		snapshot.ChannelID != (ID{}) {
		return nil, fmt.Errorf("new incoming bridge already binds a " +
			"channel")
	}
	snapshot.Phase = PaymentRegistered
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}

	return &paymentBridgeState{snapshot: snapshot.Clone()}, nil
}

// SamePaymentBridgeTerms reports whether two snapshots describe the same
// reservation while ignoring lifecycle state advanced by bridge workers.
func SamePaymentBridgeTerms(a, b PaymentBridgeSnapshot) bool {
	if a.Direction != b.Direction || a.PaymentHash != b.PaymentHash ||
		a.ClientNodeKey != b.ClientNodeKey ||
		a.ReservedSCID != b.ReservedSCID ||
		a.SourceAmount != b.SourceAmount ||
		a.DestinationAmount != b.DestinationAmount ||
		a.ServerFee != b.ServerFee ||
		a.RoutingFeeBudget != b.RoutingFeeBudget ||
		a.PublicInvoice != b.PublicInvoice {
		return false
	}

	// The outgoing source channel is selected before registration. An
	// incoming destination channel is selected only after its public HTLC
	// is intercepted, so it is lifecycle state rather than a reservation
	// term.
	return a.Direction != PaymentOutgoing || a.ChannelID == b.ChannelID
}

// RestorePaymentBridgeState validates one durable bridge snapshot.
func RestorePaymentBridgeState(snapshot PaymentBridgeSnapshot) (
	PaymentBridgeState, error) {

	if err := snapshot.Validate(); err != nil {
		return nil, err
	}

	return &paymentBridgeState{snapshot: snapshot.Clone()}, nil
}

// Snapshot returns an isolated durable snapshot.
func (s *paymentBridgeState) Snapshot() PaymentBridgeSnapshot {
	return s.snapshot.Clone()
}

// String returns the durable bridge phase.
func (s *paymentBridgeState) String() string {
	return s.snapshot.Phase.String()
}

// IsTerminal reports whether the direct bridge owns more work.
func (s *paymentBridgeState) IsTerminal() bool {
	return s.snapshot.Phase.IsTerminal()
}

// ProcessEvent applies one bridge fact without executing I/O.
func (s *paymentBridgeState) ProcessEvent(_ context.Context,
	event PaymentBridgeEvent, _ *PaymentBridgeEnvironment) (
	*protofsm.StateTransition[
		PaymentBridgeEvent, PaymentBridgeAction,
		*PaymentBridgeEnvironment,
	], error) {

	if event == nil {
		return nil, fmt.Errorf("payment bridge event is required")
	}

	next := s.snapshot.Clone()
	changed, err := applyPaymentBridgeEvent(&next, event)
	if err != nil {
		return nil, err
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}

	transition := &protofsm.StateTransition[
		PaymentBridgeEvent, PaymentBridgeAction,
		*PaymentBridgeEnvironment,
	]{
		NextState: &paymentBridgeState{
			snapshot: next,
		},
	}
	if !changed {
		return transition, nil
	}
	action := PendingPaymentBridgeAction(next)
	if action != nil {
		transition.NewEvents = fn.Some(protofsm.EmittedEvent[
			PaymentBridgeEvent, PaymentBridgeAction,
		]{
			Outbox: []PaymentBridgeAction{action},
		})
	}

	return transition, nil
}

// PendingPaymentBridgeAction returns the replayable side effect for a phase.
func PendingPaymentBridgeAction(
	snapshot PaymentBridgeSnapshot) PaymentBridgeAction {

	switch snapshot.Phase {
	case PaymentRegistered:
		if snapshot.Direction == PaymentOutgoing {
			return &PreparePaymentSource{}
		}

	case PaymentSourceReady:
		return &WaitPaymentSource{}

	case PaymentSourceLocked, PaymentDestinationInFlight:
		return &DispatchPaymentDestination{}

	case PaymentPreimageKnown:
		return &SettlePaymentSource{Preimage: *snapshot.Preimage}

	case PaymentSourceFailing:
		return &FailPaymentSource{Reason: snapshot.Failure}

	case PaymentCompleted, PaymentFailed, PaymentVHTLCFallback,
		PaymentNeedsIntervention:
		return nil
	}

	return nil
}

// applyPaymentBridgeEvent dispatches one immutable bridge fact to its narrow
// transition handler.
func applyPaymentBridgeEvent(next *PaymentBridgeSnapshot,
	event PaymentBridgeEvent) (bool, error) {

	switch event := event.(type) {
	case *PaymentSourcePrepared:
		return applyPaymentSourcePrepared(next)

	case *PaymentSourceUnavailable:
		return applyPaymentSourceUnavailable(next, event)

	case *PaymentSourceHTLCLocked:
		return applyPaymentSourceLocked(next, event)

	case *PaymentDestinationStarted:
		return applyPaymentDestinationStarted(next, event)

	case *PaymentDestinationSettled:
		return applyPaymentDestinationSettled(next, event)

	case *PaymentSourceSettled:
		return applyPaymentSourceSettled(next)

	case *PaymentDestinationFailed:
		return applyPaymentDestinationFailed(next, event)

	case *PaymentSourceFailed:
		return applyPaymentSourceFailed(next)

	case *PaymentFallbackSelected:
		return applyPaymentFallbackSelected(next, event)

	case *PaymentInterventionRequired:
		return applyPaymentInterventionRequired(next, event)

	default:
		return false, fmt.Errorf("unknown payment bridge event %T",
			event)
	}
}

// applyPaymentSourcePrepared records that the outgoing hold invoice exists.
func applyPaymentSourcePrepared(next *PaymentBridgeSnapshot) (bool, error) {
	if next.Direction != PaymentOutgoing {
		return false, fmt.Errorf("incoming source needs no invoice")
	}
	if next.Phase == PaymentSourceReady {
		return false, nil
	}
	if next.Phase != PaymentRegistered {
		return false, fmt.Errorf("cannot prepare source from %s",
			next.Phase)
	}
	next.Phase = PaymentSourceReady

	return true, nil
}

// applyPaymentSourceUnavailable starts failure of an unavailable outgoing
// source before any destination payment exists.
func applyPaymentSourceUnavailable(next *PaymentBridgeSnapshot,
	event *PaymentSourceUnavailable) (bool, error) {

	if next.Direction != PaymentOutgoing {
		return false, fmt.Errorf("incoming source cannot be " +
			"unavailable")
	}
	if next.Phase == PaymentSourceFailing || next.Phase == PaymentFailed {
		if next.Failure == event.Reason {
			return false, nil
		}

		return false, fmt.Errorf("payment source already failed " +
			"differently")
	}
	if next.Phase != PaymentSourceReady {
		return false, fmt.Errorf("cannot fail source from %s",
			next.Phase)
	}
	if event.Reason == "" {
		return false, fmt.Errorf("source failure reason required")
	}
	next.Failure = event.Reason
	next.Phase = PaymentSourceFailing

	return true, nil
}

// applyPaymentSourceLocked binds the exact held source HTLC to the bridge.
func applyPaymentSourceLocked(next *PaymentBridgeSnapshot,
	event *PaymentSourceHTLCLocked) (bool, error) {

	if next.Phase == PaymentSourceLocked ||
		next.Phase == PaymentDestinationInFlight {

		if paymentCircuitsEqual(next.Circuit, event.Circuit) {
			return false, nil
		}

		return false, fmt.Errorf("payment source already bound")
	}
	if next.Direction == PaymentOutgoing {
		if next.Phase != PaymentSourceReady || event.Circuit != nil {
			return false, fmt.Errorf("invalid outgoing source lock")
		}
	} else if next.Phase != PaymentRegistered || event.Circuit == nil {
		return false, fmt.Errorf("invalid incoming source lock")
	}
	if event.Circuit != nil {
		circuit := *event.Circuit
		next.Circuit = &circuit
	}
	next.Phase = PaymentSourceLocked

	return true, nil
}

// applyPaymentDestinationStarted binds the selected active channel and alias.
func applyPaymentDestinationStarted(next *PaymentBridgeSnapshot,
	event *PaymentDestinationStarted) (bool, error) {

	if next.Phase == PaymentDestinationInFlight {
		if next.ChannelID == event.ChannelID &&
			next.DestinationSCID == event.DestinationSCID {
			return false, nil
		}

		return false, fmt.Errorf("payment destination already bound")
	}
	if next.Phase != PaymentSourceLocked {
		return false, fmt.Errorf("cannot start destination from %s",
			next.Phase)
	}
	if event.ChannelID == (ID{}) || event.DestinationSCID == 0 {
		return false, fmt.Errorf("active destination channel required")
	}
	if next.Direction == PaymentOutgoing &&
		(next.ChannelID != event.ChannelID ||
			next.ReservedSCID != event.DestinationSCID) {
		return false, fmt.Errorf("outgoing destination changed channel")
	}
	next.ChannelID = event.ChannelID
	next.DestinationSCID = event.DestinationSCID
	next.Phase = PaymentDestinationInFlight

	return true, nil
}

// applyPaymentDestinationSettled persists the preimage before source release.
func applyPaymentDestinationSettled(next *PaymentBridgeSnapshot,
	event *PaymentDestinationSettled) (bool, error) {

	if event.Preimage.Hash() != next.PaymentHash {
		return false, fmt.Errorf("destination preimage mismatch")
	}
	if next.Phase == PaymentPreimageKnown ||
		next.Phase == PaymentCompleted {

		if next.Preimage != nil && *next.Preimage == event.Preimage {
			return false, nil
		}

		return false, fmt.Errorf("payment already has another preimage")
	}
	if next.Phase != PaymentDestinationInFlight {
		return false, fmt.Errorf("cannot settle destination from %s",
			next.Phase)
	}
	preimage := event.Preimage
	next.Preimage = &preimage
	next.Phase = PaymentPreimageKnown

	return true, nil
}

// applyPaymentSourceSettled records completion after applying the preimage.
func applyPaymentSourceSettled(next *PaymentBridgeSnapshot) (bool, error) {
	if next.Phase == PaymentCompleted {
		return false, nil
	}
	if next.Phase != PaymentPreimageKnown {
		return false, fmt.Errorf("cannot settle source from %s",
			next.Phase)
	}
	next.Phase = PaymentCompleted

	return true, nil
}

// applyPaymentDestinationFailed starts source failure while no preimage exists.
func applyPaymentDestinationFailed(next *PaymentBridgeSnapshot,
	event *PaymentDestinationFailed) (bool, error) {

	if next.Preimage != nil {
		return false, fmt.Errorf("cannot fail after preimage is known")
	}
	if next.Phase == PaymentSourceFailing || next.Phase == PaymentFailed {
		if next.Failure == event.Reason {
			return false, nil
		}

		return false, fmt.Errorf("payment already failed differently")
	}
	if next.Phase != PaymentSourceLocked &&
		next.Phase != PaymentDestinationInFlight {
		return false, fmt.Errorf("cannot fail destination from %s",
			next.Phase)
	}
	if event.Reason == "" {
		return false, fmt.Errorf("destination failure reason required")
	}
	next.Failure = event.Reason
	next.Phase = PaymentSourceFailing

	return true, nil
}

// applyPaymentSourceFailed records that both payment legs failed safely.
func applyPaymentSourceFailed(next *PaymentBridgeSnapshot) (bool, error) {
	if next.Phase == PaymentFailed {
		return false, nil
	}
	if next.Phase != PaymentSourceFailing {
		return false, fmt.Errorf("cannot fail source from %s",
			next.Phase)
	}
	next.Phase = PaymentFailed

	return true, nil
}

// applyPaymentFallbackSelected hands a pristine incoming payment to vHTLCs.
func applyPaymentFallbackSelected(next *PaymentBridgeSnapshot,
	event *PaymentFallbackSelected) (bool, error) {

	if next.Direction != PaymentIncoming {
		return false, fmt.Errorf("only incoming payment can fall back")
	}
	if next.Phase == PaymentVHTLCFallback {
		if next.Failure == event.Reason {
			return false, nil
		}

		return false, fmt.Errorf("fallback reason changed")
	}
	if next.Phase != PaymentRegistered &&
		next.Phase != PaymentSourceLocked &&
		next.Phase != PaymentDestinationInFlight {
		return false, fmt.Errorf("cannot select fallback from %s",
			next.Phase)
	}
	if event.Reason == "" {
		return false, fmt.Errorf("fallback reason required")
	}
	next.Failure = event.Reason
	next.Phase = PaymentVHTLCFallback

	return true, nil
}

// applyPaymentInterventionRequired preserves a known preimage for recovery.
func applyPaymentInterventionRequired(next *PaymentBridgeSnapshot,
	event *PaymentInterventionRequired) (bool, error) {

	if next.Preimage == nil {
		return false, fmt.Errorf("intervention requires known preimage")
	}
	if event.Reason == "" {
		return false, fmt.Errorf("intervention reason required")
	}
	if next.Phase == PaymentNeedsIntervention {
		if next.Failure == event.Reason {
			return false, nil
		}

		return false, fmt.Errorf("intervention reason changed")
	}
	if next.Phase != PaymentPreimageKnown {
		return false, fmt.Errorf("cannot require intervention from %s",
			next.Phase)
	}
	next.Failure = event.Reason
	next.Phase = PaymentNeedsIntervention

	return true, nil
}

func paymentCircuitsEqual(a, b *PaymentCircuit) bool {
	if a == nil || b == nil {
		return a == b
	}

	return *a == *b
}
