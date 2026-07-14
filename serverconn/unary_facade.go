package serverconn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const errMalformedResponseBody = "failed to unmarshal response body: %w"

// UnaryFacade implements mailboxrpc.RPCClient by delegating send and response
// delivery through a ServerConnectionActor. The send path calls Edge.Send
// directly (synchronous, no actor mailbox) for low-latency unary RPCs. The
// await path registers a waiter with the connector's in-memory response
// registry, which the ingress loop signals when a KIND_RESPONSE envelope
// arrives.
//
// Unary RPC sends do not need durable egress — callers retry on failure. The
// durable egress path (SendClientEventRequest through DurableActor.Tell) is
// reserved for FSM outbox messages from the round actor.
type UnaryFacade struct {
	// connector is the server connection actor that owns the response
	// registry and configuration.
	connector *ServerConnectionActor
}

// NewUnaryFacade creates a new unary RPC facade backed by the given
// ServerConnectionActor.
func NewUnaryFacade(
	connector *ServerConnectionActor,
) *UnaryFacade {

	return &UnaryFacade{
		connector: connector,
	}
}

// SendRPC builds an RPC request envelope, sends it via the mailbox edge, and
// returns the correlation and idempotency identifiers so the caller can
// subsequently await the response.
func (f *UnaryFacade) SendRPC(ctx context.Context,
	method mailboxrpc.ServiceMethod, req proto.Message,
	opts mailboxrpc.RPCOptions) (mailboxrpc.SendResult, error) {

	// Once the connector is incompatible, fail new unary sends with the
	// cached error without contacting the edge.
	if ce := f.connector.compatibilityError(); ce != nil {
		return mailboxrpc.SendResult{}, ce
	}

	cfg := f.connector.cfg

	msgID, err := randomID(16)
	if err != nil {
		return mailboxrpc.SendResult{}, fmt.Errorf("generate "+
			"msg id: %w", err)
	}

	idempotencyKey := opts.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey, err = randomID(16)
		if err != nil {
			return mailboxrpc.SendResult{}, fmt.Errorf("generate "+
				"idempotency key: %w", err)
		}
	}

	correlationID := opts.CorrelationID
	if correlationID == "" {
		correlationID = idempotencyKey
	}

	// Pre-register the response waiter before sending so a fast response
	// pulled by the ingress loop between Send and AwaitRPC is buffered
	// rather than dropped. The returned Future is not needed here —
	// AwaitRPC re-registers (idempotently) and blocks on it.
	corrID := CorrelationID(correlationID)
	f.connector.RegisterWaiter(corrID)

	body, err := anypb.New(req)
	if err != nil {
		f.connector.removeWaiter(corrID)

		return mailboxrpc.SendResult{}, fmt.Errorf("wrap request "+
			"in Any: %w", err)
	}

	envelope := &mailboxpb.Envelope{
		MsgId:           msgID,
		IdempotencyKey:  idempotencyKey,
		Sender:          cfg.LocalMailboxID,
		Recipient:       cfg.RemoteMailboxID,
		CreatedAtUnixMs: time.Now().UnixMilli(),
		Headers:         cfg.mergeAuthHeaders(opts.Headers),
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_REQUEST,
			Service:       method.Service,
			Method:        method.Method,
			CorrelationId: correlationID,
			ReplyTo:       cfg.LocalMailboxID,
		},
	}

	resp, err := cfg.Edge.Send(ctx, &mailboxpb.SendRequest{
		Envelope: envelope,
	})
	if sendErr := edgeResponseError(
		"send rpc request", resp, err,
	); sendErr != nil {

		f.connector.removeWaiter(corrID)
		f.connector.checkPermanentStatus(ctx, sendErr)

		f.connector.log.WarnS(ctx, "Unary send failed",
			sendErr,
			slog.String("service", method.Service),
			slog.String("method", method.Method),
		)

		return mailboxrpc.SendResult{}, sendErr
	}

	f.connector.log.TraceS(
		ctx, "Sent unary RPC request",
		slog.String("service", method.Service),
		slog.String("method", method.Method),
		slog.String("correlation_id", correlationID),
	)

	return mailboxrpc.SendResult{
		CorrelationID:  correlationID,
		IdempotencyKey: idempotencyKey,
	}, nil
}

// AwaitRPC registers a waiter for the given correlation ID and blocks until
// the ingress loop delivers a KIND_RESPONSE envelope, the waiter expires,
// or the context is cancelled. The response envelope body is unmarshaled
// into resp.
func (f *UnaryFacade) AwaitRPC(ctx context.Context, correlationID string,
	resp proto.Message) error {

	corrID := CorrelationID(correlationID)

	// Idempotent re-registration: if SendRPC already registered this
	// ID, we get back the same (possibly already completed) Future.
	future := f.connector.RegisterWaiter(corrID)
	defer f.connector.removeWaiter(corrID)

	// Recheck after registering to close the race with a concurrent
	// markIncompatible: if incompatibility was cached before this register,
	// we observe it here and return without blocking; if it lands after,
	// the waiter is already in the registry and FailAll completes it.
	// Either ordering returns the cached error rather than blocking
	// forever.
	if ce := f.connector.compatibilityError(); ce != nil {
		return ce
	}

	result := future.Await(ctx)
	if result.IsErr() {
		return result.Err()
	}

	var env *mailboxpb.Envelope
	result.WhenOk(func(e *mailboxpb.Envelope) {
		env = e
	})

	if env == nil {
		return fmt.Errorf("response waiter completed without delivery")
	}

	// Check for a server-side gRPC status error before inspecting
	// the body. This covers servers that set error headers with or
	// without a populated body field.
	if rpcErr := mailboxrpc.DecodeErrorHeaders(
		env.Headers,
	); rpcErr != nil {
		return rpcErr
	}

	if env.Body == nil {
		return fmt.Errorf("response envelope has nil body")
	}

	// The body is an anypb.Any containing the response proto.
	// Unmarshal the raw Value bytes directly into the caller's
	// response message, discarding unknown fields for forward
	// compatibility. We intentionally skip TypeUrl validation here:
	// the generated stubs always pass the correct concrete type, and
	// a server-side type mismatch would surface as garbled fields
	// rather than silent data loss (proto3 zero-values).
	err := (proto.UnmarshalOptions{
		DiscardUnknown: true,
	}).Unmarshal(env.Body.Value,
		resp,
	)
	if err != nil {
		return fmt.Errorf(errMalformedResponseBody, err)
	}

	return nil
}

// randomID generates a cryptographically random hex-encoded identifier.
func randomID(nbytes int) (string, error) {
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf), nil
}

// Compile-time interface check.
var _ mailboxrpc.RPCClient = (*UnaryFacade)(nil)
