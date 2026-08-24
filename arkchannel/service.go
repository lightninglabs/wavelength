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
	coordinator *Coordinator
	executor    ActionExecutor
}

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
func NewService(coordinator *Coordinator,
	executor ActionExecutor) (*Service, error) {

	if coordinator == nil {
		return nil, fmt.Errorf("channel coordinator is required")
	}
	if executor == nil {
		return nil, fmt.Errorf("channel action executor is required")
	}

	service := &Service{
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

	return s.Apply(ctx, id, &OORPreparationStarted{})
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

	record, _, err = s.coordinator.Apply(ctx, id, &BindVTXO{
		Binding: binding,
	})

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

// Apply records one callback fact before executing any resulting action.
func (s *Service) Apply(ctx context.Context, id ID, event Event) (Record,
	error) {

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

// RecordChannelEvent persists one fact without executing its resulting action.
// Paired endpoint protocols use this as a barrier so both databases contain an
// irreversible close artifact before either side publishes or archives it.
func (s *Service) RecordChannelEvent(ctx context.Context, id ID, event Event) (
	Record, error) {

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

	return s.Apply(ctx, id, &RequestCooperativeClose{Request: request})
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

	return s.Apply(ctx, id, &Materialize{})
}

// ObserveFundingFinalized records lnd's existing pending-open notification by
// its durable channel point. A missed notification can be recovered through
// ReconcileFunding.
func (s *Service) ObserveFundingFinalized(ctx context.Context, party Party,
	channelPoint wire.OutPoint) (Record, error) {

	record, err := s.coordinator.FindByChannelPoint(ctx, channelPoint)
	if err != nil {
		return Record{}, err
	}

	return s.Apply(ctx, record.Snapshot.Terms.ID, &FundingFinalized{
		Party: party,
	})
}

// ReconcileFunding repairs missed pending-open notifications from lnd's
// authoritative channel database.
func (s *Service) ReconcileFunding(ctx context.Context, party Party,
	source FundingFinalizationSource) error {

	if source == nil {
		return fmt.Errorf("funding finalization source is required")
	}
	if party != PartyClient && party != PartyHub {
		return fmt.Errorf("local channel party is required")
	}
	records, err := s.coordinator.ListNonTerminal(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		snapshot := record.Snapshot
		if snapshot.Backing == nil || partyFinalized(snapshot, party) ||
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
		if _, err := s.Apply(
			ctx, snapshot.Terms.ID, &FundingFinalized{
				Party: party,
			},
		); err != nil {
			return err
		}
	}

	return nil
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
