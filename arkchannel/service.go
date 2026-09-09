package arkchannel

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
)

// FundingFinalizationSource reports whether lnd durably stores both initial
// commitments for an exact channel point.
type FundingFinalizationSource interface {
	FundingFinalized(context.Context, Terms, Backing) (bool, error)
}

// Service is the small application boundary for promotion and receive intent
// coordination. Native lnd remains behind the ActionExecutor.
type Service struct {
	localParty  Party
	coordinator *Coordinator
	executor    ActionExecutor
}

// eventOrigin identifies whether a fact came from this endpoint or its
// authenticated channel peer.
type eventOrigin uint8

const (
	eventOriginLocal eventOrigin = iota + 1
	eventOriginPeer
)

// ResumeFailure identifies one channel whose already-durable action could not
// be replayed during process startup.
type ResumeFailure struct {
	ChannelID ID
	Err       error
}

// ResumeFailures reports isolated per-channel replay failures after every
// resumable channel has been attempted.
type ResumeFailures struct {
	Failures []ResumeFailure
}

// Error summarizes the channels that could not resume.
func (e *ResumeFailures) Error() string {
	if e == nil || len(e.Failures) == 0 {
		return "Ark channel resume failed"
	}
	if len(e.Failures) == 1 {
		failure := e.Failures[0]

		return fmt.Sprintf("resume Ark channel %x: %v",
			failure.ChannelID[:4], failure.Err)
	}

	return fmt.Sprintf("%d Ark channels failed to resume", len(e.Failures))
}

// Unwrap exposes each underlying action failure to errors.Is and errors.As.
func (e *ResumeFailures) Unwrap() []error {
	if e == nil {
		return nil
	}

	errs := make([]error, 0, len(e.Failures))
	for _, failure := range e.Failures {
		errs = append(errs, failure.Err)
	}

	return errs
}

// NewService constructs an Ark channel coordination service.
func NewService(localParty Party, coordinator *Coordinator,
	executor ActionExecutor) (*Service, error) {

	if localParty != PartyClient && localParty != PartyHub {
		return nil, fmt.Errorf("local channel party is required")
	}
	if coordinator == nil {
		return nil, fmt.Errorf("channel coordinator is required")
	}
	if executor == nil {
		return nil, fmt.Errorf("channel action executor is required")
	}

	service := &Service{
		localParty:  localParty,
		coordinator: coordinator,
		executor:    executor,
	}
	if binder, ok := executor.(ChannelEventSinkBinder); ok {
		if err := binder.BindChannelEventSink(service); err != nil {
			return nil, err
		}
	}

	return service, nil
}

// RegisterReceiveIntent durably reserves a future hub-funded OOR channel.
func (s *Service) RegisterReceiveIntent(ctx context.Context, terms Terms) (
	Record, error) {

	if terms.Kind != KindReceiveIntent {
		return Record{}, fmt.Errorf("receive intent terms are required")
	}

	return s.coordinator.Request(ctx, terms)
}

// RegisterPromotion durably records an existing-VTXO channel before either
// endpoint starts native lnd funding.
func (s *Service) RegisterPromotion(ctx context.Context, terms Terms) (Record,
	error) {

	if terms.Kind != KindPromotion {
		return Record{}, fmt.Errorf("promotion terms are required")
	}

	return s.coordinator.Request(ctx, terms)
}

// StartOORPreparation durably arms wallet selection before the funder asks the
// OOR actor to construct the channel-policy output.
func (s *Service) StartOORPreparation(ctx context.Context, id ID) (Record,
	error) {

	return s.ApplyLocalEvent(ctx, id, &OORPreparationStarted{})
}

// RecordPreparedOOR validates and durably attaches the exact prepared OOR
// output without executing the resulting native funding action.
func (s *Service) RecordPreparedOOR(ctx context.Context, id ID,
	binding VTXOBinding) (Record, error) {

	validator, ok := s.executor.(interface {
		ValidatePreparedOOR(context.Context, Terms, VTXOBinding) error
	})
	if !ok {
		return Record{}, fmt.Errorf("channel executor cannot " +
			"validate prepared OOR transfers")
	}
	record, err := s.coordinator.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if err := validator.ValidatePreparedOOR(
		ctx, record.Snapshot.Terms, binding,
	); err != nil {
		return Record{}, fmt.Errorf("validate prepared OOR: %w", err)
	}

	record, err = s.recordEvent(ctx, id, &BindVTXO{
		Binding: binding,
	}, eventOriginLocal)

	return record, err
}

// BindPreparedOOR records the exact prepared output before resuming native
// funding. A crash between these calls leaves the funding action replayable.
func (s *Service) BindPreparedOOR(ctx context.Context, id ID,
	binding VTXOBinding) (Record, error) {

	if _, err := s.RecordPreparedOOR(ctx, id, binding); err != nil {
		return Record{}, err
	}

	return s.ResumeChannelAction(ctx, id)
}

// PromoteVTXO registers and negotiates a channel backed by a prepared OOR
// transfer from an existing client-funded VTXO.
func (s *Service) PromoteVTXO(ctx context.Context, terms Terms,
	binding VTXOBinding) (Record, error) {

	if terms.Kind != KindPromotion {
		return Record{}, fmt.Errorf("promotion terms are required")
	}
	if _, err := s.RegisterPromotion(ctx, terms); err != nil {
		return Record{}, err
	}
	if _, err := s.BindPreparedOOR(ctx, terms.ID, binding); err != nil {
		return Record{}, err
	}

	return s.coordinator.Get(ctx, terms.ID)
}

// ApplyLocalEvent records a trusted local callback before executing any
// resulting action.
func (s *Service) ApplyLocalEvent(ctx context.Context, id ID, event Event) (
	Record, error) {

	return s.applyEvent(ctx, id, event, eventOriginLocal)
}

// ApplyPeerEvent records a fact received from the authenticated channel peer
// before executing any resulting action.
func (s *Service) ApplyPeerEvent(ctx context.Context, id ID, event Event) (
	Record, error) {

	return s.applyEvent(ctx, id, event, eventOriginPeer)
}

// applyEvent validates event authority, persists the fact, and executes the
// action derived from the resulting durable state.
func (s *Service) applyEvent(ctx context.Context, id ID, event Event,
	origin eventOrigin) (Record, error) {

	if err := s.authorizeEvent(ctx, id, event, origin); err != nil {
		return Record{}, err
	}

	record, actions, err := s.coordinator.Apply(ctx, id, event)
	if err != nil {
		return Record{}, err
	}
	if err := s.execute(ctx, id, actions); err != nil {
		return Record{}, err
	}
	if len(actions) > 0 {
		return s.coordinator.Get(ctx, id)
	}

	return record, nil
}

// RecordLocalEvent persists a local fact without executing its resulting
// action. Paired endpoint protocols use this as a durable barrier.
func (s *Service) RecordLocalEvent(ctx context.Context, id ID, event Event) (
	Record, error) {

	return s.recordEvent(ctx, id, event, eventOriginLocal)
}

// RecordPeerEvent persists an authenticated peer fact without executing its
// resulting action. Paired protocols use this only after binding the mailbox
// identity to the channel peer.
func (s *Service) RecordPeerEvent(ctx context.Context, id ID, event Event) (
	Record, error) {

	return s.recordEvent(ctx, id, event, eventOriginPeer)
}

// recordEvent validates event authority and stores the fact without executing
// the resulting action.
func (s *Service) recordEvent(ctx context.Context, id ID, event Event,
	origin eventOrigin) (Record, error) {

	if err := s.authorizeEvent(ctx, id, event, origin); err != nil {
		return Record{}, err
	}

	record, _, err := s.coordinator.Apply(ctx, id, event)

	return record, err
}

// ResumeChannelAction executes the action implied by one already durable
// channel record.
func (s *Service) ResumeChannelAction(ctx context.Context, id ID) (Record,
	error) {

	record, actions, err := s.coordinator.Resume(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if err := s.execute(ctx, id, actions); err != nil {
		return Record{}, err
	}
	if len(actions) == 0 {
		return record, nil
	}

	return s.coordinator.Get(ctx, id)
}

// RequestCooperativeClose starts a 3-of-3 OOR spend of an active channel-policy
// VTXO without materializing the unpublished lnd channel point.
func (s *Service) RequestCooperativeClose(ctx context.Context, id ID,
	request CooperativeCloseRequest) (Record, error) {

	return s.ApplyLocalEvent(
		ctx, id, &RequestCooperativeClose{
			Request: request,
		},
	)
}

// GetChannel returns the latest durable channel record without executing its
// pending action. Cross-endpoint coordinators use this after a paired barrier
// to avoid returning a stale pre-action acknowledgement.
func (s *Service) GetChannel(ctx context.Context, id ID) (Record, error) {
	return s.coordinator.Get(ctx, id)
}

// ListChannels returns every channel with a remaining runtime or recovery
// obligation owned by this endpoint.
func (s *Service) ListChannels(ctx context.Context) ([]Record, error) {
	return s.coordinator.ListNonTerminal(ctx)
}

// Materialize enters the durable on-chain handoff and resumes that exact
// action on retry if a prior process stopped after persisting the transition.
func (s *Service) Materialize(ctx context.Context, id ID) (Record, error) {
	record, err := s.coordinator.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if record.Snapshot.Phase == PhaseMaterializing {
		return s.ResumeChannelAction(ctx, id)
	}

	return s.ApplyLocalEvent(ctx, id, &Materialize{})
}

// ObserveFundingFinalized records lnd's existing pending-open notification by
// its durable channel point. A missed notification can be recovered through
// ReconcileFunding.
func (s *Service) ObserveFundingFinalized(ctx context.Context,
	channelPoint wire.OutPoint) (Record, error) {

	record, err := s.coordinator.FindByChannelPoint(ctx, channelPoint)
	if err != nil {
		return Record{}, err
	}

	return s.ApplyLocalEvent(
		ctx, record.Snapshot.Terms.ID, &FundingFinalized{
			Party: s.localParty,
		},
	)
}

// ReconcileFunding repairs missed pending-open notifications from lnd's
// authoritative channel database.
func (s *Service) ReconcileFunding(ctx context.Context,
	source FundingFinalizationSource) error {

	if source == nil {
		return fmt.Errorf("funding finalization source is required")
	}
	records, err := s.coordinator.ListNonTerminal(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		snapshot := record.Snapshot
		if snapshot.Backing == nil ||
			partyFinalized(snapshot, s.localParty) ||
			snapshot.Phase == PhaseCancelling ||
			snapshot.Phase == PhaseFailed {

			continue
		}
		finalized, err := source.FundingFinalized(
			ctx, snapshot.Terms, *snapshot.Backing,
		)
		if err != nil {
			return err
		}
		if !finalized {
			continue
		}
		if _, err := s.ApplyLocalEvent(
			ctx, snapshot.Terms.ID, &FundingFinalized{
				Party: s.localParty,
			},
		); err != nil {
			return err
		}
	}

	return nil
}

// authorizeEvent enforces which authenticated side owns every event class.
// Cryptographically complete artifacts still need an origin because the
// origin controls when a local side effect becomes replayable.
func (s *Service) authorizeEvent(ctx context.Context, id ID, event Event,
	origin eventOrigin) error {

	if event == nil {
		return fmt.Errorf("channel event is required")
	}
	record, err := s.coordinator.Get(ctx, id)
	if err != nil {
		return err
	}
	terms := record.Snapshot.Terms

	switch event := event.(type) {
	case *OORPreparationStarted, *BindVTXO, *BackingSigned,
		*OORFinalized, *OORAborted:
		return s.requireOriginParty(origin, terms.Funder, event)

	case *FundingPeerReady:
		return s.requireOriginParty(origin, PartyClient, event)

	case *FundingFinalized:
		return s.requireOriginParty(origin, event.Party, event)

	case *RecoveryPackageInstalled:
		return s.requireOriginParty(origin, event.Party, event)

	case *ReceiveIntentAbortRequested:
		return s.requireOriginParty(origin, PartyClient, event)

	case *RequestCooperativeClose, *CooperativeClosePublished,
		*CooperativeCloseAborted:
		return s.requireOriginParty(origin, PartyClient, event)

	case *CooperativeCloseSigned:
		return s.requireOriginParty(origin, event.Party, event)

	case *CooperativeCloseFinalized:
		return s.requireOriginParty(origin, event.Party, event)

	case *ExpirePrePONR, *FundingCanceled, *ChannelActive, *Materialize,
		*SourceSpent, *BackingPublished, *BackingObserved,
		*ChannelClosed, *Fail:

		if origin != eventOriginLocal {
			return fmt.Errorf("%T requires local subsystem "+
				"evidence", event)
		}

		return nil

	default:
		return fmt.Errorf("unknown channel event %T", event)
	}
}

// requireOriginParty verifies that an event came from the side whose evidence
// it claims to carry.
func (s *Service) requireOriginParty(origin eventOrigin, expected Party,
	event Event) error {

	actual := s.localParty
	if origin == eventOriginPeer {
		actual = otherParty(s.localParty)
	}
	if actual != expected {
		return fmt.Errorf("%T from %s cannot assert %s evidence", event,
			actual, expected)
	}

	return nil
}

// otherParty returns the only authenticated counterparty for one endpoint.
func otherParty(local Party) Party {
	if local == PartyClient {
		return PartyHub
	}

	return PartyClient
}

// partyFinalized reports the local acknowledgement stored in one snapshot.
func partyFinalized(snapshot Snapshot, party Party) bool {
	switch party {
	case PartyClient:
		return snapshot.ClientFinalized

	case PartyHub:
		return snapshot.HubFinalized

	default:
		return false
	}
}

// Resume replays side effects implied by all resumable durable records.
func (s *Service) Resume(ctx context.Context) error {
	work, err := s.coordinator.ResumeAll(ctx)
	if err != nil {
		return err
	}
	failures := make([]ResumeFailure, 0)
	for _, item := range work {
		id := item.Record.Snapshot.Terms.ID
		if err := s.executor.Execute(ctx, id, item.Action); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures = append(failures, ResumeFailure{
				ChannelID: id,
				Err:       err,
			})
		}
	}
	if len(failures) > 0 {
		return &ResumeFailures{Failures: failures}
	}

	return nil
}

// execute performs already-durable actions in order.
func (s *Service) execute(ctx context.Context, id ID, actions []Action) error {
	for _, action := range actions {
		if err := s.executor.Execute(ctx, id, action); err != nil {
			return err
		}
	}

	return nil
}
