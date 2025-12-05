package clientconn

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"google.golang.org/protobuf/proto"
)

// ClientID is a unique identifier for a client.
type ClientID string

// ClientMessage is an interface that server rounds FSM outbox messages must
// implement to send messages to clients. This allows conversion to proto
// messages without creating import cycles.
type ClientMessage interface {
	// ClientID returns the identifier of the client to send the message to.
	ClientID() ClientID

	// ToProto converts the message to a protobuf message that can be sent
	// over gRPC.
	ToProto() proto.Message
}

// ClientConnMsg is the sealed interface for messages that can be sent to the
// ServerConnectionActor. These are typically FSM outbox messages from the
// client that need to be relayed to the server.
type ClientConnMsg interface {
	actor.Message
	clientsConnMsgSealed()
}

// ClientConnResp is the sealed interface for responses from the
// ClientsConnectionActor.
type ClientConnResp interface {
	actor.Message
	clientsConnRespSealed()
}

// SendServerEventRequest wraps a server rounds FSM outbox message and requests
// it be sent to the appropriate client. The actor will convert it to the
// appropriate proto message and send via gRPC.
type SendServerEventRequest struct {
	actor.BaseMessage

	// Message is the server rounds FSM outbox message to send to clients.
	// It must implement the ClientMessage interface which provides the
	// ToProto() method for conversion to protobuf.
	Message ClientMessage
}

func (m *SendServerEventRequest) MessageType() string {
	return "SendServerEventRequest"
}

func (m *SendServerEventRequest) clientsConnMsgSealed() {}

// SendClientEventResponse acknowledges that the message was sent.
type SendClientEventResponse struct {
	actor.BaseMessage

	Success bool
	Error   string
}

func (m *SendClientEventResponse) MessageType() string {
	return "SendClientEventResponse"
}

func (m *SendClientEventResponse) clientsConnRespSealed() {}

// ClientsConnectionActor is a simple relay actor that accepts server round FSM
// outbox messages and sends them to clients via gRPC or other transport. This
// decouples the server round FSM from the transport layer.
type ClientsConnectionActor struct {
}

// NewClientsConnectionActor creates a new clients connection actor.
// TODO: Add parameters for gRPC connection and client actor reference.
func NewClientsConnectionActor() *ClientsConnectionActor {
	return &ClientsConnectionActor{}
}

// Receive processes incoming messages.
func (a *ClientsConnectionActor) Receive(ctx context.Context,
	msg ClientConnMsg) fn.Result[ClientConnResp] {

	switch m := msg.(type) {
	case *SendServerEventRequest:
		return a.handleSendClientEvent(ctx, m)

	default:
		return fn.Err[ClientConnResp](fmt.Errorf(
			"unknown message type: %T", msg))
	}
}

// handleSendClientEvent converts a server FSM outbox message to a proto
// message and sends it to the appropriate client.
func (a *ClientsConnectionActor) handleSendClientEvent(ctx context.Context,
	req *SendServerEventRequest) fn.Result[ClientConnResp] {

	// Convert the message to proto using the ServerMessage interface.
	protoMsg := req.Message.ToProto()

	// TODO: Send the proto message via gRPC. For now, this is a no-op.
	// In production, this would:
	//  1. Type switch on protoMsg to determine gRPC method.
	//  2. Call the appropriate grpcClient method.
	//  3. Handle response and notify the server rounds actor.
	_ = protoMsg

	return fn.Ok[ClientConnResp](&SendClientEventResponse{
		Success: true,
	})
}
