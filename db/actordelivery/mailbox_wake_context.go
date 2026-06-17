package actordelivery

import (
	"context"
)

// mailboxEnqueueFlagKey is the context key for the per-transaction flag that
// records whether ANY mailbox received a message inside an ExecTx. ExecTx reads
// it after commit to decide whether to fire the coarse mailbox wake. The wake
// is coarse -- it rouses every registered mailbox, not a specific one -- so a
// single boolean is all the transaction needs to track; it does not record
// which mailbox was enqueued.
type mailboxEnqueueFlagKey struct{}

// withMailboxEnqueueFlag returns a context carrying a fresh per-transaction
// mailbox-enqueue flag alongside a pointer to it. ExecTx installs the flag so
// any enqueue executed within the transaction can mark it, regardless of which
// store path the enqueue takes: the tx-scoped TxActorDeliveryStore and the
// folded outbox path's (*Store).EnqueueMessage (which joins the ambient tx via
// TransactionExecutor.ExecTx) both call noteMailboxEnqueued on the same ctx.
func withMailboxEnqueueFlag(ctx context.Context) (context.Context, *bool) {
	enqueued := new(bool)

	return context.WithValue(
		ctx, mailboxEnqueueFlagKey{}, enqueued,
	), enqueued
}

// noteMailboxEnqueued records that some mailbox received a message in the
// transaction carried by ctx, if a flag is present. A transaction body runs on
// a single goroutine, so the unsynchronized write is safe. It is a no-op
// outside an ExecTx (no flag in context), so the non-transactional enqueue path
// is unaffected.
func noteMailboxEnqueued(ctx context.Context) {
	if enqueued, ok := ctx.Value(mailboxEnqueueFlagKey{}).(*bool); ok {
		*enqueued = true
	}
}
