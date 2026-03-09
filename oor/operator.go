package oor

import (
	"context"
	"fmt"
	"time"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo-client/rpc/oorpb"
	"github.com/lightninglabs/darepo/clientconn"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	// oorServiceName is the protobuf service name used in mailbox
	// envelope routing for the OOR RPC service.
	oorServiceName = "oorpb.OORMailboxService"

	// oorSenderMailboxID is the default server identity stamped on
	// response envelopes sent by the OOR operator.
	oorSenderMailboxID = "svc:oor"

	// oorResponseMsgPrefix prefixes mailbox response envelope IDs.
	oorResponseMsgPrefix = "resp-"
)

// OOROperatorConfig holds dependencies for the OOR operator
// dispatcher factory.
type OOROperatorConfig struct {
	// Edge is the mailbox client for sending response envelopes
	// back to clients.
	Edge mailboxpb.MailboxServiceClient

	// SenderMailboxID is the identity stamped on response
	// envelopes. Defaults to oorSenderMailboxID.
	SenderMailboxID string

	// OORActor is the OOR transfer coordinator. Inbound requests
	// are forwarded to the actor for processing.
	OORActor *Actor
}

// OOROperator provides OORMailboxService RPC dispatchers for the
// per-client clientconn ingress loops. It follows the same pattern
// as the round and indexer operators: translate inbound mailbox
// envelopes into service calls and send responses via the edge.
type OOROperator struct {
	cfg OOROperatorConfig
	mux *mailboxrpc.ServeMux
}

// NewOOROperator creates a new OOR operator and registers the OOR
// RPC handlers on a ServeMux.
func NewOOROperator(
	cfg OOROperatorConfig) (*OOROperator, error) {

	if cfg.Edge == nil {
		return nil, fmt.Errorf("edge is required")
	}
	if cfg.SenderMailboxID == "" {
		cfg.SenderMailboxID = oorSenderMailboxID
	}

	mux := mailboxrpc.NewServeMux()
	op := &OOROperator{cfg: cfg, mux: mux}

	oorpb.RegisterOORMailboxServiceMailboxServer(mux, op)

	return op, nil
}

// Dispatchers returns the EnvelopeDispatcher map for all OOR
// service RPC methods.
func (o *OOROperator) Dispatchers() clientconn.DispatcherMap {
	methods := []string{
		"SubmitPackage",
		"FinalizePackage",
	}

	dm := make(clientconn.DispatcherMap, len(methods))
	for _, method := range methods {
		key := mailboxrpc.ServiceMethod{
			Service: oorServiceName,
			Method:  method,
		}

		dm[key] = o.makeDispatcher(method)
	}

	return dm
}

// makeDispatcher creates an EnvelopeDispatcher closure for a single
// OOR RPC method.
func (o *OOROperator) makeDispatcher(
	method string) clientconn.EnvelopeDispatcher {

	return func(ctx context.Context,
		env *mailboxpb.Envelope) error {

		if env == nil || env.Rpc == nil {
			return nil
		}
		if env.Body == nil {
			return fmt.Errorf("missing request body")
		}

		resp, handlerErr := o.mux.ServeRPC(
			ctx, env.Rpc.Service, method,
			env.Body.Value,
		)

		replyTo := env.Rpc.ReplyTo
		if replyTo == "" {
			replyTo = env.Sender
		}

		responseEnv := &mailboxpb.Envelope{
			ProtocolVersion: env.ProtocolVersion,
			MsgId:           oorResponseMsgPrefix + env.MsgId,
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

// SubmitPackage implements oorpb.OORMailboxServiceMailboxServer.
// The request is forwarded to the OOR actor for session creation
// and PSBT signing.
func (o *OOROperator) SubmitPackage(_ context.Context,
	_ *oorpb.SubmitPackageRequest) (
	*oorpb.SubmitPackageResponse, error) {

	// TODO(roasbeef): Convert proto request to domain type and
	// submit to OOR actor as SubmitOORRequest.
	return &oorpb.SubmitPackageResponse{}, nil
}

// FinalizePackage implements oorpb.OORMailboxServiceMailboxServer.
// The request is forwarded to the OOR actor for session
// finalization.
func (o *OOROperator) FinalizePackage(_ context.Context,
	_ *oorpb.FinalizePackageRequest) (
	*oorpb.FinalizePackageResponse, error) {

	// TODO(roasbeef): Convert proto request to domain type and
	// submit to OOR actor as FinalizeOORRequest.
	return &oorpb.FinalizePackageResponse{}, nil
}

// Compile-time check that OOROperator implements the mailbox server
// interface.
var _ oorpb.OORMailboxServiceMailboxServer = (*OOROperator)(nil)
