package oor

import (
	"context"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/serverconn"
)

// OutboxHandler executes FSM outbox requests and returns follow-up events. The
// FSM stays pure and emits outbox events; a handler implements the I/O behind
// this boundary. The per-session actor reuses LocalPersistenceOutboxHandler for
// incoming VTXO materialization and the metadata recipient filter.
type OutboxHandler interface {
	Handle(ctx context.Context, sessionID SessionID,
		outbox OutboxEvent) ([]Event, error)
}

// incomingMetadataFilter is the recipient-filter view the per-session actor
// asserts on its IncomingHandler when building the metadata query.
type incomingMetadataFilter = IncomingMetadataRecipientFilter

// serverConnOutboxCodec encodes serverconn messages for the durable cross-actor
// outbox written during a session's commit.
var serverConnOutboxCodec = serverconn.NewServerConnCodec()

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
	codec.MustRegister(actor.RestartTLVType, func() actor.TLVMessage {
		return &actor.RestartMessage{}
	})

	return codec
}
