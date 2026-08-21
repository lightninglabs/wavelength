package arkchannel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
)

var (
	// ErrNotFound indicates that a channel ID is not registered.
	ErrNotFound = errors.New("ark channel not found")

	// ErrConflict indicates that another worker advanced the channel first.
	ErrConflict = errors.New("ark channel revision conflict")

	// ErrOORPreparationAmbiguous means the prepared-transfer request may
	// have reached its durable actor even though the caller did not receive
	// the resulting binding. The channel must remain recoverable until a
	// keyed lookup either reconstructs and aborts that transfer or
	// preparation succeeds.
	ErrOORPreparationAmbiguous = errors.New("ark channel OOR preparation " +
		"outcome is ambiguous")
)

const maxCASAttempts = 8

// Record wraps a snapshot with its compare-and-swap revision.
type Record struct {
	Snapshot         Snapshot
	Revision         uint64
	CreatedAt        time.Time
	UpdatedAt        time.Time
	PrePONRStartedAt time.Time
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

	GetByPendingChannelID(context.Context, [32]byte) (Record, error)

	GetByChannelPoint(context.Context, wire.OutPoint) (Record, error)

	ListNonTerminal(context.Context) ([]Record, error)

	CompareAndSwap(context.Context, ID, uint64, Snapshot) (Record, error)
}

// RecordObserver projects already-durable channel facts into ancillary state.
// Observers must fail safe internally because they cannot veto an FSM commit.
type RecordObserver interface {
	ObserveArkChannel(context.Context, Record)
}

// Coordinator persists transitions before returning replayable side effects.
type Coordinator struct {
	store     Store
	observers []RecordObserver
}

// NewCoordinator constructs a durable Ark-channel coordinator.
func NewCoordinator(store Store,
	observers ...RecordObserver) (*Coordinator, error) {

	if store == nil {
		return nil, fmt.Errorf("channel store is required")
	}
	for _, observer := range observers {
		if observer == nil {
			return nil, fmt.Errorf("channel record observer is " +
				"required")
		}
	}

	return &Coordinator{store: store, observers: observers}, nil
}

// Request registers immutable terms idempotently.
func (c *Coordinator) Request(ctx context.Context, terms Terms) (Record,
	error) {

	state, err := NewState(terms)
	if err != nil {
		return Record{}, err
	}
	existing, err := c.store.GetByPendingChannelID(
		ctx, terms.PendingChannelID,
	)
	switch {
	case err == nil:
		if existing.Snapshot.Terms.ID != terms.ID {
			return Record{}, fmt.Errorf("pending channel ID " +
				"already belongs to another Ark channel")
		}
		if err := sameTerms(
			existing.Snapshot.Terms, terms,
		); err != nil {
			return Record{}, err
		}

		return existing, nil

	case !errors.Is(err, ErrNotFound):
		return Record{}, err
	}

	record, err := c.store.Create(ctx, state.Snapshot())
	if err == nil {
		c.observe(ctx, record)

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

// ListNonTerminal loads channels that still need recovery or observation,
// including closed cooperative channels with a source-defense obligation.
func (c *Coordinator) ListNonTerminal(ctx context.Context) ([]Record, error) {
	return c.store.ListNonTerminal(ctx)
}

// FindByPendingChannelID resolves a live channel intent for lnd's inbound
// funding acceptor. Pending channel IDs are immutable and may identify only
// one non-terminal Ark workflow.
func (c *Coordinator) FindByPendingChannelID(ctx context.Context,
	pendingID [32]byte) (Record, error) {

	return c.store.GetByPendingChannelID(ctx, pendingID)
}

// FindByChannelPoint resolves the durable backing record observed by lnd's
// pending-channel lifecycle.
func (c *Coordinator) FindByChannelPoint(ctx context.Context,
	channelPoint wire.OutPoint) (Record, error) {

	return c.store.GetByChannelPoint(ctx, channelPoint)
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
			c.observe(ctx, record)

			return record, emittedActions(transition), nil
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
		c.observe(ctx, updated)

		return updated, emittedActions(transition), nil
	}

	return Record{}, nil, fmt.Errorf("advance channel %x: %w", id[:4],
		ErrConflict)
}

// observe notifies best-effort projections only after the channel row commits.
func (c *Coordinator) observe(ctx context.Context, record Record) {
	for _, observer := range c.observers {
		observer.ObserveArkChannel(ctx, record)
	}
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

// ResumeAll loads resumable records and returns their replayable work.
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
	if a.Phase != b.Phase ||
		a.OORPreparationStarted != b.OORPreparationStarted ||
		a.ClientFinalized != b.ClientFinalized ||
		a.HubFinalized != b.HubFinalized ||
		a.OORFinalized != b.OORFinalized ||
		a.OORAborted != b.OORAborted ||
		a.RecoveryReady != b.RecoveryReady ||
		a.BackingPublished != b.BackingPublished ||
		a.ClientCloseSigned != b.ClientCloseSigned ||
		a.HubCloseSigned != b.HubCloseSigned ||
		a.ClientCloseFinalized != b.ClientCloseFinalized ||
		a.HubCloseFinalized != b.HubCloseFinalized ||
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
	if (a.SourceConflict == nil) != (b.SourceConflict == nil) {
		return false
	}
	if a.SourceConflict != nil &&
		(a.SourceConflict.OutPoint != b.SourceConflict.OutPoint ||
			a.SourceConflict.SpendingTxID !=
				b.SourceConflict.SpendingTxID) {
		return false
	}
	if (a.CooperativeCloseRequest == nil) !=
		(b.CooperativeCloseRequest == nil) ||
		(a.CooperativeClose == nil) != (b.CooperativeClose == nil) {
		return false
	}
	if a.CooperativeCloseRequest != nil &&
		!cooperativeCloseRequestsEqual(
			*a.CooperativeCloseRequest, *b.CooperativeCloseRequest,
		) {
		return false
	}
	if a.CooperativeClose != nil && !cooperativeClosesEqual(
		*a.CooperativeClose, *b.CooperativeClose,
	) {
		return false
	}

	return true
}
