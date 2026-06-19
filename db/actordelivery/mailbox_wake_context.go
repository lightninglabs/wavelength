package actordelivery

import (
	"context"
)

// mailboxEnqueueSetKey is the context key for the per-transaction set that
// records WHICH mailboxes received a message inside an ExecTx. ExecTx reads it
// after commit to fire a targeted wake at exactly those mailboxes' consumers,
// instead of a coarse broadcast to every registered mailbox. Tracking the set
// (rather than a single boolean) is what lets the post-commit wake skip the
// idle mailboxes that received nothing in this transaction -- the dominant cost
// under load is every durable actor re-polling on every unrelated commit.
type mailboxEnqueueSetKey struct{}

// withMailboxEnqueueSet returns a context carrying a fresh per-transaction set
// of enqueued mailbox IDs alongside the set itself. ExecTx installs it so any
// enqueue executed within the transaction can record its target mailbox,
// regardless of which store path the enqueue takes: the tx-scoped
// TxActorDeliveryStore and the folded outbox path's (*Store).EnqueueMessage
// (which joins the ambient tx via TransactionExecutor.ExecTx) both call
// noteMailboxEnqueued on the same ctx. A map is reference-typed, so the value
// stored in the context and the value returned to ExecTx share one backing
// store; a single-goroutine transaction body mutates it without a lock.
func withMailboxEnqueueSet(ctx context.Context) (
	context.Context, map[string]struct{}) {

	enqueued := make(map[string]struct{})

	return context.WithValue(
		ctx, mailboxEnqueueSetKey{}, enqueued,
	), enqueued
}

// noteMailboxEnqueued records that mailboxID received a message in the
// transaction carried by ctx, if a set is present. A transaction body runs on a
// single goroutine, so the unsynchronized map write is safe. It is a no-op
// outside an ExecTx (no set in context), so the non-transactional enqueue path
// is unaffected.
func noteMailboxEnqueued(ctx context.Context, mailboxID string) {
	set, ok := ctx.Value(mailboxEnqueueSetKey{}).(map[string]struct{})
	if !ok {
		return
	}

	set[mailboxID] = struct{}{}
}
