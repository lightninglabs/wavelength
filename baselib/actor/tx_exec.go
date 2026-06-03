package actor

import (
	"context"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// ErrLeaseLost indicates that a fenced Commit aborted because the message's
// lease was no longer held by this consumer. This happens when the behavior
// stalled past its lease (e.g. a slow side-effect IO) and the message was
// reclaimed and reprocessed elsewhere. The Commit transaction rolls back and
// applies nothing; any duplicate effect the stale consumer would have emitted
// is deduplicated downstream by the receiver's ON CONFLICT (id) DO NOTHING.
var ErrLeaseLost = fmt.Errorf("lease lost: message reclaimed by another " +
	"consumer")

// Exec is the per-message execution handle handed to a TxBehavior. It lets a
// behavior do slow side-effect IO without holding the SQLite writer, then group
// several writes into a single short, fenced transaction.
//
// The type parameter S is the behavior's typed, transaction-scoped store,
// produced by the StoreFactory passed to NewTxBehavior. Inside Read/Commit the
// store joins the framework transaction (its DB calls run against the same
// *sql.Tx via the context), so domain writes and the framework's
// ack/dedup bookkeeping commit atomically together.
type Exec[S any] interface {
	// Read runs fn inside a short read-only transaction (a WAL snapshot)
	// and acquires no writer lock. Use it to load inputs before doing IO.
	Read(ctx context.Context,
		fn func(ctx context.Context, store S) error) error

	// Commit runs fn inside one short writer transaction. Every store call
	// inside fn is atomic together. The framework folds the lease-fenced
	// ack and the dedup mark into this same transaction, so the behavior's
	// state advance and the message's consumption are one exactly-once
	// unit. If the lease was lost while the behavior did IO, Commit returns
	// ErrLeaseLost and the whole transaction is rolled back.
	Commit(ctx context.Context,
		fn func(ctx context.Context, store S) error) error
}

// StoreFactory builds the behavior's typed, transaction-scoped store S from the
// framework's transaction-scoped DeliveryStore. The DeliveryStore exposes the
// framework primitives (e.g. EnqueueOutbox) for effect emission; the supplied
// context carries the *sql.Tx so domain stores join the same transaction. A
// factory that only needs domain stores can ignore both arguments and return a
// store value captured at construction (those stores join via the context that
// Read/Commit pass to the closure).
//
// Atomicity contract: the returned store MUST perform its writes against the
// per-call transaction. Domain stores do this by reading the *sql.Tx off the
// context that Read/Commit hand to the closure (via actor.TxFromContext). Two
// ways to silently break this -- both commit out-of-band with no error while
// the framework's ack still fences, so state is NOT atomic with consumption:
//
//	// WRONG: captures an outer (non-tx) context, so writes do not join the
//	// Commit transaction.
//	factory := func(context.Context, actor.DeliveryStore) myStore {
//	    return myStore{ctx: outerCtx, db: db}
//	}
//
//	// WRONG: returns a store bound to the raw DB handle, bypassing the tx.
//	factory := func(context.Context, actor.DeliveryStore) myStore {
//	    return myStore{db: rawDB}
//	}
//
//	// RIGHT: the store reads the tx from the closure's ctx at call time.
//	factory := func(context.Context, actor.DeliveryStore) myStore {
//	    return myStore{domain: domainStore} // joins via ctx in Commit's fn
//	}
//
// In short: never capture a context in the factory; let the store pick up the
// transaction from the ctx passed into the Read/Commit closure.
type StoreFactory[S any] func(ctx context.Context, ds DeliveryStore) S

// TxBehavior is an opt-in alternative to ActorBehavior for durable actors that
// must do side-effect IO outside the writer transaction. Instead of the
// framework wrapping the whole Receive in one transaction (the implicit
// processInTransaction path), the behavior drives its own Read/Commit phases
// via the Exec handle. The classic ActorBehavior path is unchanged and remains
// the safe default for behaviors that do no IO.
type TxBehavior[M TLVMessage, R any, S any] interface {
	// Receive processes a message using the Exec handle for explicit
	// Read/Commit phases. Side-effect IO runs between Read and Commit,
	// while no transaction is held.
	Receive(ctx context.Context, msg M, ax Exec[S]) fn.Result[R]
}

// execCore is the non-generic engine backing every Exec[S]. It is bound to a
// single in-flight delivery and folds the lease fence, dedup mark, and ack into
// the Commit transaction so the store type S can stay a free type parameter
// without leaking into the DurableActor.
type execCore struct {
	// store is the transaction-aware delivery store used to open the short
	// Read/Commit transactions.
	store TxAwareDeliveryStore

	// msgID is the delivery (mailbox message) ID being processed.
	msgID string

	// leaseToken is the lease this consumer holds on the message; Commit's
	// ack is fenced on it.
	leaseToken string

	// actorID identifies the owning actor for the dedup mark.
	actorID string

	// dedupTTL is how long the processed-message marker is retained.
	dedupTTL time.Duration

	// committed records whether a Commit transaction succeeded. When true,
	// the state write, dedup mark, and fenced ack are already durable, so
	// the actor must not ack again.
	committed bool
}

// read runs fn inside a short read-only transaction.
func (e *execCore) read(ctx context.Context,
	fn func(ctx context.Context, ds DeliveryStore) error) error {

	return e.store.ExecTx(ctx, true, func(txCtx context.Context,
		s DeliveryStore) error {

		return fn(txCtx, s)
	})
}

// commit runs fn inside one short writer transaction, then folds in the dedup
// mark and the lease-fenced ack. A zero-row ack means the lease was reclaimed
// while the behavior did IO, so the whole transaction is rolled back and
// ErrLeaseLost is returned. On success it records that the delivery has been
// consumed durably.
func (e *execCore) commit(ctx context.Context,
	fn func(ctx context.Context, ds DeliveryStore) error) error {

	err := e.store.ExecTx(ctx, false, func(txCtx context.Context,
		s DeliveryStore) error {

		// Fence first: ack deletes the mailbox row only if our lease
		// token still matches. Zero rows means the lease was reclaimed
		// while the behavior did IO, so we abort and apply nothing --
		// the whole transaction rolls back, including any writes the
		// closure would make below. Fencing up front keeps the
		// stale-lease path a true no-op rather than relying on rollback
		// alone.
		rows, err := s.AckMessage(txCtx, e.msgID, e.leaseToken)
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrLeaseLost
		}

		// The behavior's own writes (state advance, outbox effects)
		// join this transaction via txCtx, so they commit atomically
		// with the ack above.
		if err := fn(txCtx, s); err != nil {
			return err
		}

		// Mark the message processed for dedup within the same tx so
		// the state advance and the dedup record are atomic with
		// consumption.
		return s.MarkProcessed(txCtx, e.msgID, e.actorID, e.dedupTTL)
	})
	if err != nil {
		return err
	}

	e.committed = true

	return nil
}

// execHandle is the generic Exec[S] adapter over the non-generic execCore. It
// builds the typed store S from the transaction-scoped DeliveryStore on each
// Read/Commit and hands it to the behavior's closure.
type execHandle[S any] struct {
	core    *execCore
	factory StoreFactory[S]
}

// Read implements Exec by running the closure against a typed store inside a
// read-only transaction.
func (e execHandle[S]) Read(ctx context.Context,
	fn func(ctx context.Context, store S) error) error {

	return e.core.read(ctx, func(txCtx context.Context,
		ds DeliveryStore) error {

		return fn(txCtx, e.factory(txCtx, ds))
	})
}

// Commit implements Exec by running the closure against a typed store inside
// the fenced writer transaction.
func (e execHandle[S]) Commit(ctx context.Context,
	fn func(ctx context.Context, store S) error) error {

	return e.core.commit(ctx, func(txCtx context.Context,
		ds DeliveryStore) error {

		return fn(txCtx, e.factory(txCtx, ds))
	})
}

// BoundTxBehavior is a store-type-erased durable behavior produced by
// NewTxBehavior. The store type parameter S is captured inside, so the value
// fits in DurableActorConfig.TxBehavior without forcing the store type onto the
// DurableActor itself. Construct one with NewTxBehavior and place it in the
// config to run the actor on the Read/Commit execution path.
type BoundTxBehavior[M TLVMessage, R any] struct {
	// run drives one message through the behavior using an Exec[S] built
	// over the supplied execCore. It is nil for the zero value.
	run func(ctx context.Context, core *execCore, msg M) fn.Result[R]
}

// isSet reports whether this BoundTxBehavior carries a behavior.
func (b BoundTxBehavior[M, R]) isSet() bool {
	return b.run != nil
}

// NewTxBehavior binds a TxBehavior and its StoreFactory into a
// store-type-erased BoundTxBehavior suitable for DurableActorConfig.TxBehavior.
// The factory is the injection point for the behavior's typed store: it is
// invoked inside each Read/Commit transaction to produce the store handed to
// the behavior's closures.
func NewTxBehavior[M TLVMessage, R any, S any](
	behavior TxBehavior[M, R, S],
	factory StoreFactory[S],
) BoundTxBehavior[M, R] {

	return BoundTxBehavior[M, R]{
		run: func(ctx context.Context, core *execCore,
			msg M) fn.Result[R] {

			ax := execHandle[S]{
				core:    core,
				factory: factory,
			}

			return behavior.Receive(ctx, msg, ax)
		},
	}
}
