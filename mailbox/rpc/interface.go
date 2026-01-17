package mailboxrpc

import (
	"context"

	"google.golang.org/protobuf/proto"
)

// HandlerFunc handles an RPC request payload and returns a response payload.
//
// Implementations must assume at-least-once delivery. Callers may retry the
// same semantic operation, so handlers must be idempotent under the envelope's
// idempotency key.
type HandlerFunc func(context.Context, proto.Message) (proto.Message, error)

// Router registers handlers for envelopes addressed to a protobuf service and
// method name.
//
// The (service, method) identifiers use the protobuf fully-qualified service
// name:
//   - service: "<proto package>.<ServiceName>"
//     (example: "arkrpc.v1.RoundService")
//   - method:  "<MethodName>" (example: "JoinRound")
type Router interface {
	// Handle registers a typed handler for a single RPC method.
	//
	// newReq must return a pointer to the protobuf request type.
	//
	// The router unmarshals the incoming envelope body into the returned
	// message. It should use a forward-compatible unmarshal mode (discard
	// unknown fields).
	Handle(service string, method string, newReq func() proto.Message,
		fn HandlerFunc)
}

// RPCClient sends RPC-over-mailbox requests and awaits correlated responses.
//
// Implementations are expected to ensure concurrent in-flight calls are safe
// under cursor-based acking by demultiplexing pulled responses by correlation
// id before advancing any cursor.
type RPCClient interface {
	// SendRPC sends a request payload and returns the correlation id and
	// idempotency key used for the send.
	SendRPC(ctx context.Context, service string, method string,
		req proto.Message, opts RPCOptions) (correlationID string,
		idempotencyKey string, err error)

	// AwaitRPC blocks until a response for correlationID is received.
	//
	// It then unmarshals the response into resp.
	AwaitRPC(ctx context.Context, correlationID string,
		resp proto.Message) error
}
