package indexer

import (
	"github.com/lightninglabs/darepo/clientconn"
	"google.golang.org/protobuf/proto"
)

// indexerEventMessage implements clientconn.ClientMessage for indexer
// notification events (IncomingOOREvent and IncomingVTXOEvent). The
// bridge routes the message to the correct per-client DurableActor
// based on the clientID, and the actor serializes the proto via
// ToProto() for envelope construction.
type indexerEventMessage struct {
	// clientID identifies the target client. This is the principal's
	// mailbox ID retrieved from the receive-script registration.
	clientID clientconn.ClientID

	// event is the proto event payload to deliver to the client. This
	// is either *arkrpc.IncomingOOREvent or *arkrpc.IncomingVTXOEvent.
	event proto.Message
}

// ClientID returns the target client identifier for bridge routing.
func (m *indexerEventMessage) ClientID() clientconn.ClientID {
	return m.clientID
}

// ToProto returns the proto event payload for envelope body
// construction. The per-client DurableActor wraps this in anypb.Any
// before sending.
func (m *indexerEventMessage) ToProto() proto.Message {
	return m.event
}

// Compile-time interface check.
var _ clientconn.ClientMessage = (*indexerEventMessage)(nil)
