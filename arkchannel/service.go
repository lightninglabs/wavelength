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

// NewService constructs an Ark channel coordination service.
func NewService(coordinator *Coordinator,
	executor ActionExecutor) (*Service, error) {

	if coordinator == nil {
		return nil, fmt.Errorf("channel coordinator is required")
	}
	if executor == nil {
		return nil, fmt.Errorf("channel action executor is required")
	}

	return &Service{
		coordinator: coordinator,
		executor:    executor,
	}, nil
}

// RegisterReceiveIntent durably reserves a future hub-funded channel.
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

// BindVTXO attaches the exact validated source and starts native funding.
func (s *Service) BindVTXO(ctx context.Context, id ID, binding VTXOBinding) (
	Record, error) {

	return s.Apply(ctx, id, &BindVTXO{
		Binding: binding,
	})
}

// PromoteVTXO registers and negotiates an existing client-funded VTXO.
func (s *Service) PromoteVTXO(ctx context.Context, terms Terms,
	binding VTXOBinding) (Record, error) {

	if terms.Kind != KindPromotion {
		return Record{}, fmt.Errorf("promotion terms are required")
	}
	if _, err := s.RegisterPromotion(ctx, terms); err != nil {
		return Record{}, err
	}
	if _, err := s.BindVTXO(ctx, terms.ID, binding); err != nil {
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

// Materialize asks the unroller to publish ancestry before the backing.
func (s *Service) Materialize(ctx context.Context, id ID) (Record, error) {
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

// Resume replays side effects implied by all non-terminal durable records.
func (s *Service) Resume(ctx context.Context) error {
	work, err := s.coordinator.ResumeAll(ctx)
	if err != nil {
		return err
	}
	for _, item := range work {
		id := item.Record.Snapshot.Terms.ID
		if err := s.executor.Execute(ctx, id, item.Action); err != nil {
			return err
		}
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
