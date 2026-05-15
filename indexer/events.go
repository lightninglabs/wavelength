package indexer

import (
	"github.com/lightninglabs/darepo-client/arkrpc"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo/clientconn"
	"google.golang.org/protobuf/proto"
)

const (
	// arkServiceName is the protobuf service name for
	// ArkService events pushed to clients (IncomingOOR,
	// IncomingVTXO). This differs from indexerServiceName
	// which is used for IndexerService request-response RPCs.
	arkServiceName = "arkrpc.ArkService"

	// MethodIncomingOOR is the routing method for incoming OOR
	// transfer notifications.
	MethodIncomingOOR = "IncomingOOR"

	// MethodIncomingVTXO is the routing method for incoming VTXO
	// event notifications.
	MethodIncomingVTXO = "IncomingVTXO"
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

// ServiceMethod returns the routing key for client-side ingress
// dispatch. The method is derived from the concrete proto event
// type so the client's EventRouter can dispatch IncomingOOREvent
// and IncomingVTXOEvent to the correct handlers.
func (m *indexerEventMessage) ServiceMethod() mailboxrpc.ServiceMethod {
	switch m.event.(type) {
	case *arkrpc.IncomingOOREvent:
		return mailboxrpc.ServiceMethod{
			Service: arkServiceName,
			Method:  MethodIncomingOOR,
		}

	case *arkrpc.IncomingVTXOEvent:
		return mailboxrpc.ServiceMethod{
			Service: arkServiceName,
			Method:  MethodIncomingVTXO,
		}

	default:
		return mailboxrpc.ServiceMethod{}
	}
}

// CorrelationKey returns the empty string. Indexer event notifications
// are independent fan-outs to a client (a new VTXO arrived, an OOR
// recipient event published) with no cross-event ordering invariant, so
// they participate in the global available_at order rather than a
// per-key FIFO lane.
func (m *indexerEventMessage) CorrelationKey() string { return "" }

// Compile-time interface check.
var _ clientconn.ClientMessage = (*indexerEventMessage)(nil)
