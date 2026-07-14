package oor

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/baselib/actor"
)

// OutboxHandler executes FSM outbox requests and returns follow-up events. The
// FSM stays pure and emits outbox events; a handler implements the I/O behind
// this boundary. The per-session actor reuses LocalPersistenceOutboxHandler for
// incoming VTXO materialization and the metadata recipient filter.
type OutboxHandler interface {
	Handle(ctx context.Context, sessionID SessionID,
		outbox OutboxEvent) ([]Event, error)
}

// terminalOutboxError marks a DETERMINISTIC failure while driving an FSM outbox
// effect -- one that depends only on persisted state or operator-supplied
// input, so it fails identically on every redelivery. driveOutbox converts it
// into a terminal FailEvent so the turn commits and acks the message, instead
// of returning it (which rolls the turn back and makes the durable mailbox
// redeliver a doomed retry until it dead-letters, wedging the session). A
// TRANSIENT failure -- a busy DB writer, a momentarily unavailable peer -- must
// be returned plain so the framework redelivers and the effect can succeed
// later. Only wrap an error here when re-running the identical turn could never
// succeed.
type terminalOutboxError struct {
	cause error
}

// Error returns the underlying cause's message.
func (e *terminalOutboxError) Error() string {
	return e.cause.Error()
}

// Unwrap exposes the cause so errors.As/Is see through the marker.
func (e *terminalOutboxError) Unwrap() error {
	return e.cause
}

// terminalOutboxErrorf builds a terminalOutboxError with a formatted message.
func terminalOutboxErrorf(format string, args ...any) error {
	return &terminalOutboxError{cause: fmt.Errorf(format, args...)}
}

// incomingMetadataFilter is the recipient-filter view the per-session actor
// asserts on its IncomingHandler when building the metadata query.
type incomingMetadataFilter = IncomingMetadataRecipientFilter

// newOORActorCodec registers all per-session actor message types so each
// durable actor can serialize and deserialize its mailbox messages.
func newOORActorCodec(limits ReceiveLimits) *actor.MessageCodec {
	codec := actor.NewMessageCodec()
	limits = normalizeReceiveLimits(limits)

	codec.MustRegister(
		StartTransferRequestTLVType,
		func() actor.TLVMessage {
			return &StartTransferRequest{limits: limits}
		},
	)
	codec.MustRegister(ListSessionsRequestTLVType, func() actor.TLVMessage {
		return &ListSessionsRequest{}
	})
	codec.MustRegister(DriveEventRequestTLVType, func() actor.TLVMessage {
		return &DriveEventRequest{limits: limits}
	})
	codec.MustRegister(
		ResolveIncomingTransferTLVType, func() actor.TLVMessage {
			return &ResolveIncomingTransferRequest{limits: limits}
		},
	)
	codec.MustRegister(GetStateRequestTLVType, func() actor.TLVMessage {
		return &GetStateRequest{}
	})
	codec.MustRegister(
		ResumeSessionRequestTLVType,
		func() actor.TLVMessage {
			return &ResumeSessionRequest{}
		},
	)
	codec.MustRegister(
		SessionTerminalNotificationTLVType,
		func() actor.TLVMessage {
			return &SessionTerminalNotification{}
		},
	)
	codec.MustRegister(
		RestoreNonTerminalRequestTLVType,
		func() actor.TLVMessage {
			return &RestoreNonTerminalRequest{}
		},
	)
	codec.MustRegister(actor.RestartTLVType, func() actor.TLVMessage {
		return &actor.RestartMessage{}
	})

	return codec
}
