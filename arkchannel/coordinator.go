package arkchannel

import (
	"context"
	"errors"
	"fmt"
)

var (
	// ErrNotFound indicates that a channel ID is not registered.
	ErrNotFound = errors.New("ark channel not found")

	// ErrConflict indicates that another worker advanced the channel first.
	ErrConflict = errors.New("ark channel revision conflict")
)

const maxCASAttempts = 8

// Record wraps a snapshot with its compare-and-swap revision.
type Record struct {
	Snapshot Snapshot
	Revision uint64
}

// PendingWork pairs a durable record with its replayable side effect.
type PendingWork struct {
	Record Record
	Action Action
}

// Store persists one canonical Ark coordination record per channel.
type Store interface {
	Create(context.Context, Snapshot) (Record, error)

	Get(context.Context, ID) (Record, error)

	ListNonTerminal(context.Context) ([]Record, error)

	CompareAndSwap(context.Context, ID, uint64, Snapshot) (Record, error)
}

// Coordinator persists transitions before returning replayable side effects.
type Coordinator struct {
	store Store
}

// NewCoordinator constructs a durable Ark-channel coordinator.
func NewCoordinator(store Store) (*Coordinator, error) {
	if store == nil {
		return nil, fmt.Errorf("channel store is required")
	}

	return &Coordinator{store: store}, nil
}

// Request registers immutable terms idempotently.
func (c *Coordinator) Request(ctx context.Context, terms Terms) (Record,
	error) {

	state, err := NewState(terms)
	if err != nil {
		return Record{}, err
	}
	record, err := c.store.Create(ctx, state.Snapshot())
	if err == nil {
		return record, nil
	}
	if !errors.Is(err, ErrConflict) {
		return Record{}, err
	}

	existing, getErr := c.store.Get(ctx, terms.ID)
	if getErr != nil {
		return Record{}, getErr
	}
	if err := sameTerms(existing.Snapshot.Terms, terms); err != nil {
		return Record{}, err
	}

	return existing, nil
}

// Get loads one durable channel record.
func (c *Coordinator) Get(ctx context.Context, id ID) (Record, error) {
	return c.store.Get(ctx, id)
}

// ListNonTerminal loads channels that still need recovery or observation.
func (c *Coordinator) ListNonTerminal(ctx context.Context) ([]Record, error) {
	return c.store.ListNonTerminal(ctx)
}

// Apply durably applies one event and then returns any side effect to execute.
func (c *Coordinator) Apply(ctx context.Context, id ID, event Event) (Record,
	[]Action, error) {

	for range maxCASAttempts {
		record, err := c.store.Get(ctx, id)
		if err != nil {
			return Record{}, nil, err
		}

		state, err := RestoreState(record.Snapshot)
		if err != nil {
			return Record{}, nil, err
		}
		transition, err := state.ProcessEvent(
			ctx, event, &Environment{
				ChannelID: id,
			},
		)
		if err != nil {
			return Record{}, nil, err
		}

		nextState, ok := transition.NextState.(State)
		if !ok {
			return Record{}, nil, fmt.Errorf("unexpected next "+
				"state %T", transition.NextState)
		}
		next := nextState.Snapshot()
		if snapshotsEqual(record.Snapshot, next) {
			return record, nil, nil
		}

		updated, err := c.store.CompareAndSwap(
			ctx, id, record.Revision, next,
		)
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return Record{}, nil, err
		}

		return updated, emittedActions(transition), nil
	}

	return Record{}, nil, fmt.Errorf("advance channel %x: %w", id[:4],
		ErrConflict)
}

// Resume returns the idempotent action implied by already-durable state.
func (c *Coordinator) Resume(ctx context.Context, id ID) (Record, []Action,
	error) {

	record, err := c.store.Get(ctx, id)
	if err != nil {
		return Record{}, nil, err
	}
	action, err := PendingAction(record.Snapshot)
	if err != nil {
		return Record{}, nil, err
	}
	if action == nil {
		return record, nil, nil
	}

	return record, []Action{action}, nil
}

// ResumeAll loads non-terminal records and returns their replayable work.
func (c *Coordinator) ResumeAll(ctx context.Context) ([]PendingWork, error) {
	records, err := c.store.ListNonTerminal(ctx)
	if err != nil {
		return nil, err
	}

	work := make([]PendingWork, 0, len(records))
	for _, record := range records {
		action, err := PendingAction(record.Snapshot)
		if err != nil {
			return nil, err
		}
		if action == nil {
			continue
		}

		work = append(work, PendingWork{
			Record: record,
			Action: action,
		})
	}

	return work, nil
}

// emittedActions extracts the transition outbox after its state is durable.
func emittedActions(transition *StateTransition) []Action {
	if transition.NewEvents.IsNone() {
		return nil
	}

	emitted := transition.NewEvents.UnsafeFromSome()

	return emitted.Outbox
}

// sameTerms rejects reuse of an ID for different immutable terms.
func sameTerms(a, b Terms) error {
	if a.ID != b.ID || a.Kind != b.Kind || a.Funder != b.Funder ||
		a.PendingChannelID != b.PendingChannelID ||
		a.ReservedSCID != b.ReservedSCID || a.Capacity != b.Capacity ||
		a.ClientNodeKey != b.ClientNodeKey ||
		a.HubNodeKey != b.HubNodeKey ||
		a.PaymentHash != b.PaymentHash || a.VTXO != b.VTXO {
		return fmt.Errorf("channel ID already has different terms")
	}

	return nil
}

// snapshotsEqual detects idempotent events without writing a new revision.
func snapshotsEqual(a, b Snapshot) bool {
	if err := sameTerms(a.Terms, b.Terms); err != nil {
		return false
	}
	if a.Phase != b.Phase || a.ClientFinalized != b.ClientFinalized ||
		a.HubFinalized != b.HubFinalized ||
		a.RoundCommitted != b.RoundCommitted ||
		a.RoundConfirmed != b.RoundConfirmed ||
		a.BackingPublished != b.BackingPublished ||
		a.Failure != b.Failure {
		return false
	}
	if (a.Source == nil) != (b.Source == nil) ||
		(a.Backing == nil) != (b.Backing == nil) {
		return false
	}
	if a.Source != nil && !bindingsEqual(*a.Source, *b.Source) {
		return false
	}
	if a.Backing != nil && !backingsEqual(*a.Backing, *b.Backing) {
		return false
	}

	return true
}
