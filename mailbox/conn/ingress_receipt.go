package conn

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// IngressReceiptRetention bounds transport replay protection after a durable
// local handoff. Replays do not extend the original consumption timestamp.
const IngressReceiptRetention = 30 * 24 * time.Hour

// ErrIngressReceiptMismatch means a consumed occurrence was reused for a
// different durable payload. Ingress must preserve the contradiction, not drop
// it.
var ErrIngressReceiptMismatch = errors.New("ingress receipt payload mismatch")

// ingressReceiptKey carries a network occurrence identity only as far as the
// first durable inbox insert. An in-memory Tell cannot consume this identity.
type ingressReceiptKey struct{}

// ingressReceipt tracks the current dispatch, not a previous database receipt.
// A retry gets a new token so an earlier attempt cannot prove this handoff.
type ingressReceipt struct {
	id        string
	handedOff atomic.Bool
}

// WithIngressReceipt identifies one inbound event for atomic deduplication at
// its durable inbox insert. The ID must include the local and remote mailbox
// scope and route. It is distinct from the actor's generated delivery UUID.
func WithIngressReceipt(ctx context.Context, id string) context.Context {
	return context.WithValue(
		ctx, ingressReceiptKey{}, &ingressReceipt{
			id: id,
		},
	)
}

// IngressReceiptFromContext returns the scoped network identity, when ingress
// explicitly attached one. Ordinary actor and outbox deliveries have none.
func IngressReceiptFromContext(ctx context.Context) (string, bool) {
	receipt, ok := ctx.Value(ingressReceiptKey{}).(*ingressReceipt)
	if !ok {
		return "", false
	}

	return receipt.id, receipt.id != ""
}

// NoteIngressReceiptHandoff records that this dispatch successfully inserted
// into a durable inbox or matched its retained payload. Call only after the
// enqueue operation succeeds; the enclosing transaction must still commit.
func NoteIngressReceiptHandoff(ctx context.Context) {
	if receipt, ok := ctx.Value(ingressReceiptKey{}).(*ingressReceipt); ok {
		receipt.handedOff.Store(true)
	}
}

// IngressReceiptHandedOff reports durable handoff during this dispatch. Merely
// finding an older database receipt cannot prove a recovered payload arrived.
func IngressReceiptHandedOff(ctx context.Context) bool {
	receipt, ok := ctx.Value(ingressReceiptKey{}).(*ingressReceipt)

	return ok && receipt.handedOff.Load()
}

// IsDeliveryOccurrenceID recognizes a delivery identity minted once before
// producer enqueue and reused on transport retries. Legacy body-derived IDs
// cannot distinguish identical responses to independent requests.
func IsDeliveryOccurrenceID(id string) bool {
	const prefix = "delivery-v1:"
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	value := strings.TrimPrefix(id, prefix)
	parsed, err := uuid.Parse(value)

	return err == nil && parsed.String() == value
}

// IngressQuarantine preserves an envelope that could not be adapted for local
// delivery. It is never expired automatically: transport ACK is not completion.
type IngressQuarantine struct {
	// ID identifies this exact retained envelope in its connection scope.
	ID string
	// Lane is the hash of the configured local and remote mailbox
	// identities.
	Lane string
	// Envelope contains the original serialized protobuf envelope.
	Envelope []byte
	// Reason preserves the first adaptation failure.
	Reason string
	// Attempts counts process-start recovery attempts; it never expires
	// evidence.
	Attempts int64
}

// IngressQuarantineStore is durable evidence storage used by ingress. Calls
// made with a transaction context must join that transaction.
type IngressQuarantineStore interface {
	// QuarantineIngress preserves exact wire bytes, subject to bounded
	// capacity.
	QuarantineIngress(context.Context, IngressQuarantine) error

	// ListIngressQuarantine returns retained envelopes for one connection
	// lane.
	ListIngressQuarantine(context.Context,
		string) ([]IngressQuarantine, error)

	// NoteIngressQuarantineAttempt records a bounded startup recovery
	// attempt.
	NoteIngressQuarantineAttempt(context.Context, string) error

	// DeleteIngressQuarantine removes evidence only after proven durable
	// handoff.
	DeleteIngressQuarantine(context.Context, string) error
}
