package serverconn

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"google.golang.org/protobuf/proto"
)

// ServerMessage is an interface that client FSM outbox messages must implement
// to be sent to the server. This allows conversion to proto messages without
// creating import cycles.
type ServerMessage interface {
	// ToProto converts the message to a protobuf message that can be sent
	// over gRPC.
	ToProto() proto.Message
}

// ServerConnMsg is the sealed interface for messages that can be sent to the
// ServerConnectionActor. These are typically FSM outbox messages from the
// client that need to be relayed to the server.
type ServerConnMsg interface {
	actor.Message
	serverConnMsgSealed()
}

// ServerConnResp is the sealed interface for responses from the
// ServerConnectionActor.
type ServerConnResp interface {
	actor.Message
	serverConnRespSealed()
}

// SendClientEventRequest wraps a client FSM outbox message and requests it be
// sent to the server. The actor will convert it to the appropriate proto
// message and send via gRPC.
type SendClientEventRequest struct {
	actor.BaseMessage

	// Message is the client FSM outbox message to send to the server.
	// It must implement the ServerMessage interface which provides the
	// ToProto() method for conversion to protobuf.
	Message ServerMessage
}

func (m *SendClientEventRequest) MessageType() string {
	return "SendClientEventRequest"
}

func (m *SendClientEventRequest) serverConnMsgSealed() {}

// SendClientEventResponse acknowledges that the message was sent.
type SendClientEventResponse struct {
	actor.BaseMessage

	Success bool
	Error   string
}

func (m *SendClientEventResponse) MessageType() string {
	return "SendClientEventResponse"
}

func (m *SendClientEventResponse) serverConnRespSealed() {}

// ServerConnectionActor is a simple relay actor that accepts client FSM outbox
// messages and sends them to the server via gRPC or other transport. This
// decouples the client FSM from the transport layer.
//
// The actor maintains a connection to the server and handles:
//   - Converting FSM events to proto messages.
//   - Sending messages over gRPC.
//   - Handling connection failures and retries (TODO).
//   - Notifying the client actor of server responses (TODO).
type ServerConnectionActor struct {
	// TODO: Add gRPC client connection
	// grpcClient pb.ArkServiceClient

	// TODO: Add client actor reference for sending server responses back
	// clientRef actor.TellOnlyRef[round.ServerMessageNotification]
}

// NewServerConnectionActor creates a new server connection actor.
// TODO: Add parameters for gRPC connection and client actor reference.
func NewServerConnectionActor() *ServerConnectionActor {
	return &ServerConnectionActor{}
}

// Receive processes incoming messages.
func (a *ServerConnectionActor) Receive(ctx context.Context,
	msg ServerConnMsg) fn.Result[ServerConnResp] {

	switch m := msg.(type) {
	case *SendClientEventRequest:
		return a.handleSendClientEvent(ctx, m)

	default:
		return fn.Err[ServerConnResp](fmt.Errorf(
			"unknown message type: %T", msg))
	}
}

// handleSendClientEvent converts a client FSM outbox message to a proto
// message and sends it to the server.
func (a *ServerConnectionActor) handleSendClientEvent(ctx context.Context,
	req *SendClientEventRequest) fn.Result[ServerConnResp] {

	// Convert the message to proto using the ServerMessage interface.
	protoMsg := req.Message.ToProto()

	// TODO: Send the proto message via gRPC. For now, this is a no-op.
	// In production, this would:
	//  1. Type switch on protoMsg to determine gRPC method.
	//  2. Call the appropriate grpcClient method.
	//  3. Handle response and notify the client actor.
	_ = protoMsg

	return fn.Ok[ServerConnResp](&SendClientEventResponse{
		Success: true,
	})
}

// Start initializes the server connection.
// TODO: Establish gRPC connection to server.
func (a *ServerConnectionActor) Start() error {
	return nil
}

// Stop cleanly shuts down the server connection.
// TODO: Close gRPC connection.
func (a *ServerConnectionActor) Stop() error {
	return nil
}
