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

	// Stage runs fn inside one short, lease-fenced writer transaction with
	// no dedup mark and no ack. Use it to durably advance behavior state
	// (e.g. a checkpoint) BEFORE doing side-effect IO, when the behavior
	// must preserve a persist-before-effect invariant but the message is
	// only consumed once at the end via Commit. A Stage write is its own
	// atomic unit; it is NOT atomic with the eventual Commit. Because the
	// writer lock is released the moment Stage returns, the following IO
	// never holds it -- that is the whole point of staging ahead of a slow
	// side effect.
	//
	// Stage is fenced on the lease token: if this consumer's lease was
	// reclaimed during the IO window, Stage returns ErrLeaseLost and
	// applies nothing, so a stale consumer cannot regress a newer owner's
	// checkpoint. Behaviors that Stage must still be replay-safe: a crash
	// after a Stage but before Commit redelivers the message and re-applies
	// it against the already-advanced state, so each staged advance must be
	// idempotent (monotonic state, dedup'd downstream effects).
	Stage(ctx context.Context,
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

	// leaseDuration is how long Stage extends the lease by when it fences.
	// Stage reuses ExtendLease as its token fence, so the fenced lease
	// check and a heartbeat-style extension happen in the same transaction
	// as the staged write.
	leaseDuration time.Duration

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

// stage runs fn inside one short writer transaction that is lease-fenced but
// does NOT ack the message or write a dedup mark. It durably advances behavior
// state ahead of side-effect IO without consuming the message and without
// setting committed; the message is consumed later by commit. The staged write
// commits in its own transaction, independent of that eventual commit, so it
// survives even a commit that rolls back with ErrLeaseLost.
//
// The fence matters once an actor can have more than one consumer. If this
// consumer's lease expired during the IO window and another consumer reclaimed
// and advanced (or consumed) the same mailbox row, an unfenced stage would let
// this now-stale consumer overwrite the newer owner's checkpoint -- a lost
// update / checkpoint regression. So stage fences first by extending its own
// lease (ExtendLease validates the token): zero rows means the lease was
// reclaimed, so we return ErrLeaseLost and roll back without running fn,
// applying nothing. The extension doubles as a heartbeat folded into the write.
// In a single-consumer deployment the fence always passes; it is defense for
// the multi-consumer case.
//
// stage still never acks, so how the message is finally consumed depends on
// what the behavior returns:
//
//   - Behavior errors before/at commit: processWithExec finds committed unset
//     and falls to finishNonTx, which nacks for redelivery. The redelivered
//     message replays the staged advance against the already-persisted state,
//     so the behavior MUST be idempotent under replay.
//
//   - Behavior returns success without ever calling commit: finishNonTx acks
//     the message anyway (the same tail that consumes a RestartMessage), so it
//     is NOT redelivered -- but it is consumed WITHOUT the lease-fenced,
//     co-transactional dedup that commit folds in. A behavior that staged
//     domain state and then needs exactly-once consumption must therefore call
//     commit; returning success without it silently downgrades to an at-least-
//     once consume.
func (e *execCore) stage(ctx context.Context,
	fn func(ctx context.Context, ds DeliveryStore) error) error {

	return e.store.ExecTx(ctx, false, func(txCtx context.Context,
		s DeliveryStore) error {

		// Fence first: extending our lease validates the token. Zero
		// rows means the lease was reclaimed while the behavior did IO,
		// so a now-stale consumer must not advance durable state --
		// abort and roll back without running fn, so it cannot regress
		// a newer owner's checkpoint.
		rows, err := s.ExtendLease(
			txCtx, e.msgID, e.leaseToken, e.leaseDuration,
		)
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrLeaseLost
		}

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

// Stage implements Exec by running the closure against a typed store inside an
// unfenced writer transaction that neither acks nor dedups the message.
func (e execHandle[S]) Stage(ctx context.Context,
	fn func(ctx context.Context, store S) error) error {

	return e.core.stage(ctx, func(txCtx context.Context,
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
