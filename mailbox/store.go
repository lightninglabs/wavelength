package mailbox

import (
	"context"
)

// Store provides mailbox persistence and cursor/watermark based acking.
//
// Implementations MUST be safe for concurrent use.
type Store interface {
	// Append stores env in env.Recipient's mailbox and returns the assigned
	// event sequence number.
	//
	// Duplicate msg_id values are silently ignored: the call returns
	// (0, nil) without inserting a second copy. Callers can distinguish
	// duplicates from fresh inserts because valid sequence numbers start
	// at 1.
	Append(ctx context.Context, env *Envelope) (uint64, error)

	// Pull returns up to limit envelopes from recipient's mailbox starting
	// at cursor.
	//
	// Implementations SHOULD block until at least one envelope is available
	// or ctx is done.
	Pull(ctx context.Context, recipient string, cursor uint64, limit int) (
		[]*Envelope, uint64, error)

	// AckUpTo advances the recipient's ack cursor to cursor.
	//
	// The cursor MUST be monotonic: calls that attempt to decrease it
	// MUST be treated as no-ops.
	//
	// The cursor is the next expected event sequence, and implicitly
	// acknowledges any envelopes with event_seq < cursor.
	AckUpTo(ctx context.Context, recipient string, cursor uint64) error
}
