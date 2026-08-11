package arkchannel

import (
	"context"
	"fmt"
)

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
