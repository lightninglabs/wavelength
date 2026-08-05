package serverconn

import (
	"context"
)

// deliveredOutsideTx records the envelopes one folded dispatch has already
// handed to a bounded in-memory mailbox, so a transaction that runs its body
// again does not hand them over twice.
//
// The need for it comes from where those deliveries land. A durable enqueue
// joins the fold's write transaction and is undone with it, so replaying the
// body replays the enqueue exactly once in net effect. A TryTell into an
// in-memory mailbox is not in the transaction at all: the target actor may have
// processed the message before the transaction even tries to commit. The
// production store retries its transaction body on a retryable error
// (SQLITE_BUSY, Postgres 40001/40P01) up to ten times, and every one of those
// attempts used to re-send the whole durable partition's in-memory half —
// invisibly, since the final attempt commits and the loop reports success.
//
// Only one goroutine ever dispatches ingress, so this needs no lock, and it is
// scoped to a single runFoldedDispatch call: across re-pulls the cursor has not
// moved, redelivery is the documented at-least-once behaviour, and the target
// mailbox is the thing that has to absorb it. Do not widen the record beyond
// one invocation: a cross-cycle set would suppress the FIRST delivery of an
// envelope whose earlier cycle rolled back after the TryTell, which trades a
// benign duplicate for a silent loss.
type deliveredOutsideTx struct {
	// seqs holds the event_seq of every envelope delivered by TryTell so
	// far in this folded dispatch.
	seqs map[uint64]struct{}
}

// deliveredOutsideTxKey is the context key for the in-flight record. It is a
// distinct empty struct type so nothing else can collide with it.
type deliveredOutsideTxKey struct{}

// withDeliveredOutsideTx attaches a fresh record to ctx. The context is the
// only channel available: the dispatcher signature is fixed at the wiring
// boundary (EnvelopeDispatcher), and the delivery happens below it inside the
// store's transaction body.
func withDeliveredOutsideTx(ctx context.Context) context.Context {
	tracker := &deliveredOutsideTx{
		seqs: make(map[uint64]struct{}),
	}

	return context.WithValue(ctx, deliveredOutsideTxKey{}, tracker)
}

// deliveredOutsideTxFrom returns the record attached to ctx, or nil when there
// is none. A nil record answers every question safely, so callers on paths that
// never install one (the legacy non-transactional dispatch, and dispatchers
// driven directly by tests) need no special case.
func deliveredOutsideTxFrom(ctx context.Context) *deliveredOutsideTx {
	tracker, _ := ctx.Value(deliveredOutsideTxKey{}).(*deliveredOutsideTx)

	return tracker
}

// seen reports whether the envelope at eventSeq was already delivered outside
// the transaction during this folded dispatch.
func (d *deliveredOutsideTx) seen(eventSeq uint64) bool {
	if d == nil || eventSeq == 0 {
		return false
	}

	_, ok := d.seqs[eventSeq]

	return ok
}

// mark records an out-of-transaction delivery of the envelope at eventSeq.
//
// A zero eventSeq is not recorded. The mailbox assigns event_seq from 1
// (mailbox.proto: cursors are event_seq comparisons and zero is the
// never-acked sentinel), so a zero here means the envelope was never stamped by
// a server, and keying on it would let two unstamped envelopes in one batch
// alias onto each other. Suppressing a duplicate is worth less than never
// suppressing a first delivery.
func (d *deliveredOutsideTx) mark(eventSeq uint64) {
	if d == nil || eventSeq == 0 {
		return
	}

	d.seqs[eventSeq] = struct{}{}
}
