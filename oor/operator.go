// Server-Side OOR Operator Dispatch Pipeline
//
// The OOROperator bridges the mailbox transport layer (clientconn) and the
// OOR actor for out-of-round transfers. It follows the same multi-layer
// dispatch pattern as the rounds operator:
//
//	Mailbox Envelope (from client)
//	   │
//	   ▼
//	clientconn Ingress Loop
//	   │  Routes by {Service, Method} from envelope RpcMeta
//	   ▼
//	EnvelopeDispatcher (from makeDispatcher)
//	   │  Validates envelope, injects client ID, calls ServeMux
//	   ▼
//	ServeMux → Typed Handler (SubmitPackage, FinalizePackage)
//	   │  Proto→domain conversion, forwards to OOR actor
//	   ▼
//	OOR Actor (session FSM)
//
// The OOR operator is registered via RegisterOORMailboxServiceMailboxServer
// and its dispatchers are merged into each client's ingress loop alongside
// the rounds and indexer dispatchers in RegisterClientWithAllDispatchers.
//
// See docs/dispatch_pipeline.md for the full pipeline reference.

package oor

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
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

// oorClientIDContextKey is the context key used to inject the
// client's mailbox ID into the handler context.
type oorClientIDContextKey struct{}

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
// OOR RPC method. It injects the envelope sender as the client ID
// into the context for handler methods to retrieve.
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

		// Inject the envelope sender as the client ID.
		ctx = context.WithValue(
			ctx, oorClientIDContextKey{},
			clientconn.ClientID(env.Sender),
		)

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
// It parses the submit package request, converts the signing
// descriptors to the domain type, and forwards the request to the
// OOR actor for session creation and co-signing.
func (o *OOROperator) SubmitPackage(ctx context.Context,
	req *oorpb.SubmitPackageRequest) (
	*oorpb.SubmitPackageResponse, error) {

	// ParseSubmitPackageRequest deserializes the PSBT bytes and
	// converts the proto signing descriptors to domain types.
	arkPSBT, checkpointPSBTs, descs, err :=
		oorpb.ParseSubmitPackageRequest(req)
	if err != nil {
		return nil, fmt.Errorf(
			"parse submit package: %w", err,
		)
	}

	// Convert oorpb.SigningDescriptor to oor.VTXOSigningDescriptor.
	// The field names and types are identical.
	vtxoDescs := make(
		[]VTXOSigningDescriptor, len(descs),
	)
	for i, d := range descs {
		vtxoDescs[i] = VTXOSigningDescriptor{
			Outpoint:  d.Outpoint,
			OwnerKey:  d.OwnerKey,
			ExitDelay: d.ExitDelay,
		}
	}

	// Submit the request to the OOR actor. The actor processes
	// it through the session FSM and delivers the response
	// asynchronously via the outbox event path.
	actorMsg := &SubmitOORRequest{
		ArkPSBT:                arkPSBT,
		CheckpointPSBTs:        checkpointPSBTs,
		VTXOSigningDescriptors: vtxoDescs,
	}

	result := o.cfg.OORActor.Receive(ctx, actorMsg)
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf(
			"OOR actor submit: %w", err,
		)
	}

	return &oorpb.SubmitPackageResponse{}, nil
}

// FinalizePackage implements oorpb.OORMailboxServiceMailboxServer.
// It parses the finalize request and forwards it to the OOR actor
// for session finalization.
func (o *OOROperator) FinalizePackage(ctx context.Context,
	req *oorpb.FinalizePackageRequest) (
	*oorpb.FinalizePackageResponse, error) {

	// ParseFinalizePackageRequest deserializes the session ID
	// and checkpoint PSBTs.
	sessionHash, finalCheckpoints, err :=
		oorpb.ParseFinalizePackageRequest(req)
	if err != nil {
		return nil, fmt.Errorf(
			"parse finalize package: %w", err,
		)
	}

	actorMsg := &FinalizeOORRequest{
		SessionID:            SessionID(sessionHash),
		FinalCheckpointPSBTs: finalCheckpoints,
	}

	result := o.cfg.OORActor.Receive(ctx, actorMsg)
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf(
			"OOR actor finalize: %w", err,
		)
	}

	return &oorpb.FinalizePackageResponse{}, nil
}

// oorClientIDFromContext extracts the client ID from the context
// injected by the dispatcher.
func oorClientIDFromContext(
	ctx context.Context) clientconn.ClientID { //nolint:unused

	id, _ := ctx.Value(
		oorClientIDContextKey{},
	).(clientconn.ClientID)

	return id
}

// Compile-time check that OOROperator implements the mailbox server
// interface.
var _ oorpb.OORMailboxServiceMailboxServer = (*OOROperator)(nil)

// Ensure chainhash is used (SessionID conversion).
var _ = chainhash.Hash{}
