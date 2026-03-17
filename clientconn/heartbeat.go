package clientconn

import (
	"context"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
)

const (
	// HeartbeatService is the well-known protobuf service name for
	// client heartbeat envelopes. This must match the constant in
	// the client's serverconn package.
	HeartbeatService = "clientconn.v1.HeartbeatService"

	// HeartbeatMethod is the RPC method name for heartbeat
	// envelopes.
	HeartbeatMethod = "Heartbeat"
)

// HeartbeatServiceMethod returns the ServiceMethod key for heartbeat
// envelope dispatch.
func HeartbeatServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: HeartbeatService,
		Method:  HeartbeatMethod,
	}
}

// HeartbeatDispatcher returns a no-op EnvelopeDispatcher that accepts
// heartbeat envelopes without forwarding them anywhere. The heartbeat
// is purely a liveness signal — the ActivityMarker.MarkActive call in
// the ingress loop is the real side-effect.
func HeartbeatDispatcher() EnvelopeDispatcher {
	return func(_ context.Context,
		_ *mailboxpb.Envelope) error {

		return nil
	}
}
