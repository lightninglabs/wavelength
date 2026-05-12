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

// ServiceMethod identifies an RPC endpoint by its fully-qualified protobuf
// service name and method name.
type ServiceMethod struct {
	// Service is the fully-qualified protobuf service name
	// (e.g., "arkrpc.ArkService").
	Service string

	// Method is the protobuf method name (e.g., "GetInfo").
	Method string
}

// SendResult holds the identifiers returned by a successful SendRPC call.
type SendResult struct {
	// CorrelationID uniquely identifies the request-response pair so the
	// caller can match it with a subsequent AwaitRPC call.
	CorrelationID string

	// IdempotencyKey identifies the semantic operation for deduplication
	// by the remote mailbox.
	IdempotencyKey string
}

// RPCClient sends RPC-over-mailbox requests and awaits correlated responses.
//
// Implementations are expected to ensure concurrent in-flight calls are safe
// under cursor-based acking by demultiplexing pulled responses by correlation
// id before advancing any cursor.
type RPCClient interface {
	// SendRPC sends a request payload and returns a SendResult containing
	// the correlation id and idempotency key used for the send.
	SendRPC(ctx context.Context, method ServiceMethod, req proto.Message,
		opts RPCOptions) (SendResult, error)

	// AwaitRPC blocks until a response for correlationID is received.
	//
	// It then unmarshals the response into resp.
	AwaitRPC(ctx context.Context, correlationID string,
		resp proto.Message) error
}
