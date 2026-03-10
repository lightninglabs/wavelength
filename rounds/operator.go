package rounds

import (
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/types"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/lightninglabs/darepo/clientconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	// roundServiceName is the protobuf service name used in mailbox
	// envelope routing for the round RPC service.
	roundServiceName = roundpb.ServiceName

	// operatorSenderMailboxID is the server identity stamped on
	// response and event envelopes sent by the round operator.
	operatorSenderMailboxID = "svc:rounds"

	// responseMsgPrefix prefixes mailbox response envelope IDs.
	responseMsgPrefix = "resp-"
)

// RoundOperatorConfig holds dependencies for the rounds operator
// dispatcher factory.
type RoundOperatorConfig struct {
	// Edge is the mailbox client for sending response envelopes
	// back to clients.
	Edge mailboxpb.MailboxServiceClient

	// SenderMailboxID is the identity stamped on response
	// envelopes. Defaults to operatorSenderMailboxID.
	SenderMailboxID string

	// RoundsRef is the actor reference for the rounds actor. The
	// operator sends JoinRoundRequest and other actor messages via
	// Tell (fire-and-forget) because the round FSM delivers
	// responses asynchronously through outbox events.
	RoundsRef actor.ActorRef[ActorMsg, ActorResp]
}

// RoundOperator provides RoundService RPC dispatchers for the
// per-client clientconn ingress loops. Unlike the indexer operator
// which uses synchronous ServeMux dispatch, the round operator
// translates inbound mailbox envelopes into actor messages and fires
// them at the rounds actor via Tell. Client responses flow back
// asynchronously through the outbox event path (bridge → per-client
// DurableActor → client mailbox).
//
// For request methods (JoinRound, SubmitNonces, etc.), the dispatcher
// sends an immediate KIND_RESPONSE acknowledgment so the client's
// mailbox cursor can advance, while the actual result arrives later as
// a push event.
type RoundOperator struct {
	cfg RoundOperatorConfig
	mux *mailboxrpc.ServeMux
}

// NewRoundOperator creates a new round operator and registers the
// round RPC handlers on a ServeMux for request deserialization.
func NewRoundOperator(
	cfg RoundOperatorConfig) (*RoundOperator, error) {

	if cfg.Edge == nil {
		return nil, fmt.Errorf("edge is required")
	}
	if cfg.SenderMailboxID == "" {
		cfg.SenderMailboxID = operatorSenderMailboxID
	}

	// Register the round service handlers for proto
	// deserialization. The handler implementations are thin shims
	// that convert proto requests to actor messages.
	mux := mailboxrpc.NewServeMux()
	op := &RoundOperator{cfg: cfg, mux: mux}

	roundpb.RegisterRoundServiceMailboxServer(mux, op)

	return op, nil
}

// Dispatchers returns the EnvelopeDispatcher map for all
// RoundService RPC methods. Each dispatcher follows the same
// pattern as the indexer operator: extract principal, process via
// ServeMux, build response envelope, send via edge.
func (o *RoundOperator) Dispatchers() clientconn.DispatcherMap {
	methods := []string{
		"JoinRound",
		"SubmitNonces",
		"SubmitPartialSigs",
		"SubmitForfeitSigs",
		"SubmitVTXOForfeitSigs",
	}

	dm := make(clientconn.DispatcherMap, len(methods))
	for _, method := range methods {
		key := mailboxrpc.ServiceMethod{
			Service: roundServiceName,
			Method:  method,
		}

		dm[key] = o.makeDispatcher(method)
	}

	return dm
}

// makeDispatcher creates an EnvelopeDispatcher closure for a single
// RPC method. The closure captures the operator's mux and edge for
// processing and responding.
func (o *RoundOperator) makeDispatcher(
	method string) clientconn.EnvelopeDispatcher {

	return func(ctx context.Context,
		env *mailboxpb.Envelope) error {

		if env == nil || env.Rpc == nil {
			return nil
		}
		if env.Body == nil {
			return fmt.Errorf("missing request body")
		}

		// Process the request through the ServeMux. The mux
		// deserializes the proto and calls our handler impl
		// (JoinRound, SubmitNonces, etc.).
		resp, handlerErr := o.mux.ServeRPC(
			ctx, env.Rpc.Service, method,
			env.Body.Value,
		)

		// Determine where to send the response.
		replyTo := env.Rpc.ReplyTo
		if replyTo == "" {
			replyTo = env.Sender
		}

		responseEnv := &mailboxpb.Envelope{
			ProtocolVersion: env.ProtocolVersion,
			MsgId:           responseMsgPrefix + env.MsgId,
			Sender:          o.cfg.SenderMailboxID,
			Recipient:       replyTo,
			CreatedAtUnixMs: time.Now().UnixMilli(),
			Headers: mailboxrpc.EncodeErrorHeaders(
				handlerErr,
			),
			Rpc: &mailboxpb.RpcMeta{
				Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
				Service:       env.Rpc.Service,
				Method:        method,
				CorrelationId: env.Rpc.CorrelationId,
			},
		}

		if handlerErr == nil && resp != nil {
			respAny, err := anypb.New(resp)
			if err != nil {
				return fmt.Errorf(
					"marshal response: %w", err,
				)
			}

			responseEnv.Body = respAny
		}

		sendResp, err := o.cfg.Edge.Send(
			ctx, &mailboxpb.SendRequest{
				Envelope: responseEnv,
			},
		)
		if err != nil {
			return fmt.Errorf("send response: %w", err)
		}
		if sendResp.Status != nil && !sendResp.Status.Ok {
			return fmt.Errorf(
				"send response status: %s (%s)",
				sendResp.Status.Message,
				sendResp.Status.Code,
			)
		}

		return nil
	}
}

// JoinRound implements roundpb.RoundServiceMailboxServer. It converts
// the proto request to a domain JoinRoundRequest and fires it at the
// rounds actor. The actual client response arrives asynchronously via
// the outbox event path.
func (o *RoundOperator) JoinRound(ctx context.Context,
	req *roundpb.JoinRoundRequest) (
	*roundpb.ClientSuccessResp, error) {

	domainReq, err := joinRoundRequestFromProto(req)
	if err != nil {
		return nil, fmt.Errorf("parse join request: %w", err)
	}

	// Extract client ID from the context. The dispatcher injects
	// this via the envelope sender.
	clientID := clientIDFromContext(ctx)

	actorMsg := &JoinRoundRequest{
		ClientID: clientID,
		Request:  domainReq,
	}

	tellErr := o.cfg.RoundsRef.Tell(ctx, actorMsg)
	if tellErr != nil {
		return nil, fmt.Errorf(
			"tell rounds actor: %w", tellErr,
		)
	}

	// Return an empty success response as acknowledgment. The
	// real response (with round ID and accepted outpoints) arrives
	// via the ClientSuccessResp outbox event through the bridge.
	return &roundpb.ClientSuccessResp{}, nil
}

// SubmitNonces implements roundpb.RoundServiceMailboxServer. Nonce
// submission is forwarded to the rounds actor as a RoundMsg.
func (o *RoundOperator) SubmitNonces(ctx context.Context,
	req *roundpb.SubmitNoncesRequest) (
	*roundpb.ClientVTXOAggNonces, error) {

	// TODO(roasbeef): Convert proto nonces to domain type and
	// forward to rounds actor as a nonce submission event.
	_ = req

	return &roundpb.ClientVTXOAggNonces{}, nil
}

// SubmitPartialSigs implements roundpb.RoundServiceMailboxServer.
func (o *RoundOperator) SubmitPartialSigs(ctx context.Context,
	req *roundpb.SubmitPartialSigRequest) (
	*roundpb.ClientVTXOAggSigs, error) {

	// TODO(roasbeef): Convert proto partial sigs to domain type
	// and forward to rounds actor.
	_ = req

	return &roundpb.ClientVTXOAggSigs{}, nil
}

// SubmitForfeitSigs implements roundpb.RoundServiceMailboxServer.
func (o *RoundOperator) SubmitForfeitSigs(ctx context.Context,
	req *roundpb.SubmitForfeitSigRequest) (
	*roundpb.ClientAwaitingInputSigsResp, error) {

	// TODO(roasbeef): Convert proto forfeit sigs to domain type
	// and forward to rounds actor.
	_ = req

	return &roundpb.ClientAwaitingInputSigsResp{}, nil
}

// SubmitVTXOForfeitSigs implements roundpb.RoundServiceMailboxServer.
func (o *RoundOperator) SubmitVTXOForfeitSigs(ctx context.Context,
	req *roundpb.SubmitVTXOForfeitSigsRequest) (
	*roundpb.ClientSuccessResp, error) {

	// TODO(roasbeef): Convert proto VTXO forfeit sigs to domain
	// type and forward to rounds actor.
	_ = req

	return &roundpb.ClientSuccessResp{}, nil
}

// joinRoundRequestFromProto converts a roundpb.JoinRoundRequest to
// the domain types.JoinRoundRequest. The full proto→domain conversion
// for sub-request types (boarding, VTXO, forfeit, leave) happens in
// the roundpb convert package.
//
// TODO(roasbeef): Wire full proto→domain conversion for boarding,
// VTXO, forfeit, and leave sub-requests once the roundpb convert
// helpers are exported.
func joinRoundRequestFromProto(
	_ *roundpb.JoinRoundRequest) (*types.JoinRoundRequest, error) {

	// Placeholder: return an empty domain request. The full
	// conversion requires roundpb helpers for each sub-request
	// type (BoardingRequest, VTXORequest, ForfeitRequest,
	// LeaveRequest, JoinRoundAuth).
	return &types.JoinRoundRequest{}, nil
}

// clientIDFromContext extracts the client ID from the context. In the
// dispatcher flow, the client ID is derived from the mailbox envelope
// sender.
//
// TODO(roasbeef): Wire proper context injection in the dispatcher
// closure once principal extraction is unified across operators.
func clientIDFromContext(_ context.Context) clientconn.ClientID {
	return ""
}

// Compile-time check that RoundOperator implements the mailbox server
// interface.
var _ roundpb.RoundServiceMailboxServer = (*RoundOperator)(nil)

// Compile-time check that all client-facing outbox events satisfy
// ClientMessage for bridge delivery.
var _ clientconn.ClientMessage = (*ClientErrorResp)(nil)
var _ clientconn.ClientMessage = (*ClientSuccessResp)(nil)
var _ clientconn.ClientMessage = (*ClientAwaitingInputSigsResp)(nil)
var _ clientconn.ClientMessage = (*ClientVTXOAggNonces)(nil)
var _ clientconn.ClientMessage = (*ClientVTXOAggSigs)(nil)
var _ clientconn.ClientMessage = (*ClientBatchInfo)(nil)
var _ clientconn.ClientMessage = (*ClientRoundFailedResp)(nil)

// Ensure unused imports compile. These will be used when the handler
// stubs above are filled in.
var _ = proto.Marshal
