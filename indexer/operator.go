package indexer

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo/clientconn"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"google.golang.org/protobuf/types/known/anypb"
)

// OperatorConfig holds configuration for the indexer's clientconn-based
// operator. Unlike the old hand-rolled operator, there is no poll loop
// here: the per-client ClientRuntime ingress loops handle envelope
// pulling and dispatching. The operator only provides the dispatcher map
// and event publishing methods.
type OperatorConfig struct {
	// Log is an optional logger. When None, logging is disabled.
	Log fn.Option[btclog.Logger]

	// Edge is the gRPC client for the local mailbox edge service.
	// All dispatchers share this client for sending response envelopes.
	Edge mailboxpb.MailboxServiceClient

	// SenderMailboxID is the server identity set on response and event
	// envelopes. This is typically the server's own mailbox ID.
	SenderMailboxID string

	// ProtocolVersion is the protocol version stamped on outbound
	// envelopes.
	ProtocolVersion uint32

	// Bridge is the shared per-client connection bridge. The operator
	// publishes events to specific clients through this bridge.
	Bridge *clientconn.ClientsConnBridge
}

// Operator provides IndexerService RPC dispatchers for the per-client
// clientconn ingress loops and publishes notification events via the
// shared ClientsConnBridge.
//
// The operator replaces the old hand-rolled poll loop with dispatcher
// closures that the per-client ClientRuntime calls when envelopes
// arrive on a client's server-side mailbox. Each dispatcher extracts
// the principal from the envelope sender, processes the request via the
// ServeMux, builds a KIND_RESPONSE envelope, and sends it back to the
// client's remote mailbox.
type Operator struct {
	cfg OperatorConfig
	log btclog.Logger
	svc *Service
	mux *mailboxrpc.ServeMux
}

// NewOperator creates a new indexer operator with the given config and
// service. The operator registers the service's 8 RPC handlers with a
// ServeMux for request dispatch and validates the required config
// fields.
func NewOperator(cfg OperatorConfig, svc *Service) (*Operator, error) {
	if cfg.Edge == nil {
		return nil, fmt.Errorf("edge is required")
	}
	if cfg.SenderMailboxID == "" {
		return nil, fmt.Errorf("sender mailbox id is required")
	}
	if cfg.Bridge == nil {
		return nil, fmt.Errorf("bridge is required")
	}
	if cfg.ProtocolVersion == 0 {
		cfg.ProtocolVersion = defaultOperatorProtocolVersion
	}
	if svc == nil {
		return nil, fmt.Errorf("service is required")
	}

	mux := mailboxrpc.NewServeMux()
	arkrpc.RegisterIndexerServiceMailboxServer(mux, svc)

	return &Operator{
		cfg: cfg,
		log: cfg.Log.UnwrapOr(btclog.Disabled),
		svc: svc,
		mux: mux,
	}, nil
}

// RegisterService registers an additional mailbox service on the
// operator's internal ServeMux. This allows the operator to dispatch
// requests for services beyond IndexerService (e.g., ArkService)
// using the same response-building machinery.
//
// The register function receives the mux and should call the
// appropriate RegisterXxxMailboxServer helper.
func (o *Operator) RegisterService(register func(mux *mailboxrpc.ServeMux)) {
	register(o.mux)
}

// Dispatchers returns the EnvelopeDispatcher map for all 8
// IndexerService RPC methods. The returned map should be merged
// into each client's PerClientConfig.Dispatchers when the client
// registers with the shared bridge.
//
// Each dispatcher is a closure that:
//  1. Extracts the principal identity from the envelope's Sender
//     field.
//  2. Calls the ServeMux to process the request.
//  3. Builds a KIND_RESPONSE envelope (including error headers on
//     handler failure).
//  4. Sends the response via the shared Edge.
//
// To include dispatchers for additional services registered via
// RegisterService, call ServiceDispatchers separately and merge
// the results.
func (o *Operator) Dispatchers() clientconn.DispatcherMap {
	return o.ServiceDispatchers(
		indexerServiceName, "RegisterReceiveScript",
		"ListMyReceiveScripts", "UnregisterReceiveScript",
		"ListOORRecipientEventsByScript", "ListVTXOsByScripts",
		"GetOORSessionByTxid", "GetSubtreeByScripts",
		"ListVTXOEventsByScripts",
	)
}

// ServiceDispatchers builds a DispatcherMap for the given service and
// methods using the operator's shared response-building machinery.
// This is the generalized form of Dispatchers — callers use it to
// create dispatchers for additional services registered on the
// operator's mux via RegisterService.
func (o *Operator) ServiceDispatchers(service string,
	methods ...string) clientconn.DispatcherMap {

	dm := make(clientconn.DispatcherMap, len(methods))
	for _, method := range methods {
		key := mailboxrpc.ServiceMethod{
			Service: service,
			Method:  method,
		}

		dm[key] = o.makeDispatcher(method)
	}

	return dm
}

// makeDispatcher creates an EnvelopeDispatcher closure for a single RPC
// method. The returned closure captures the operator's mux, edge, and
// sender identity so it can process requests without a reference to the
// per-client runtime.
func (o *Operator) makeDispatcher(method string) clientconn.EnvelopeDispatcher {
	return func(ctx context.Context, env *mailboxpb.Envelope) error {
		if env == nil || env.Rpc == nil {
			return nil
		}
		if env.Body == nil {
			return fmt.Errorf("missing request body")
		}

		// Extract the principal from the envelope sender. The
		// Sender field is the client's mailbox ID, which serves
		// as the authenticated identity for all indexer queries.
		principal := Principal{MailboxID: env.Sender}
		reqCtx := ContextWithPrincipal(ctx, principal)

		resp, handlerErr := o.mux.ServeRPC(
			reqCtx, env.Rpc.Service, method, env.Body.Value,
		)

		// Determine where to send the response. Prefer ReplyTo if
		// set (bidirectional mailbox pattern), otherwise fall back
		// to the envelope sender.
		replyTo := env.Rpc.ReplyTo
		if replyTo == "" {
			replyTo = env.Sender
		}

		responseEnv := &mailboxpb.Envelope{
			ProtocolVersion: env.ProtocolVersion,
			MsgId:           responseMsgIDPrefix + env.MsgId,
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
				return fmt.Errorf("marshal response: %w", err)
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
			return fmt.Errorf("send response status: %s (%s)",
				sendResp.Status.Message, sendResp.Status.Code)
		}

		return nil
	}
}

// PublishOORRecipientEvent stores an OOR recipient event and emits a
// durable EVENT to each principal currently registered for the
// recipient script. Events are routed through the shared bridge, which
// delivers them via the per-client DurableActor mailbox.
//
// Principals that are not currently registered with the bridge are
// silently skipped. Their events accumulate in the database and will be
// delivered when the client reconnects and queries.
func (o *Operator) PublishOORRecipientEvent(ctx context.Context,
	ev *arkrpc.OORRecipientEvent) error {

	event, principals, err := o.svc.AddOORRecipientEvent(ctx, ev)
	if err != nil {
		return err
	}

	if len(principals) == 0 {
		o.log.Infof("Indexer OOR event has no active principals for "+
			"session=%x output_index=%d recipient_script=%x",
			ev.SessionId, ev.OutputIndex, ev.RecipientPkScript)

		return nil
	}

	o.log.Infof("Publishing indexer OOR event to %d principal(s) for "+
		"session=%x output_index=%d recipient_script=%x",
		len(principals), ev.SessionId, ev.OutputIndex,
		ev.RecipientPkScript)

	for _, mailboxID := range principals {
		msg := &indexerEventMessage{
			clientID: clientconn.ClientID(mailboxID),
			event:    event,
		}

		tellErr := o.cfg.Bridge.Tell(
			ctx, &clientconn.SendServerEventRequest{
				Message: msg,
			},
		)
		if tellErr != nil {
			o.log.Warnf("Indexer OOR event tell failed for client "+
				"%q: %v", mailboxID, tellErr)
		}
	}

	return nil
}

// PublishVTXOEvent stores a VTXO lifecycle event and emits a durable
// EVENT to each principal currently registered for pkScript. Events are
// routed through the shared bridge.
//
// Principals that are not currently registered with the bridge are
// silently skipped.
func (o *Operator) PublishVTXOEvent(ctx context.Context, pkScript []byte,
	evType arkrpc.VTXOEventType, outpoint *arkrpc.OutPoint,
	status arkrpc.VTXOStatus, valueSat uint64, roundID string,
	batchExpiry int32, relativeExpiry uint32, origin arkrpc.VTXOOrigin,
	commitmentTxid []byte) error {

	incoming, principals, err := o.svc.AddVTXOEvent(
		ctx, pkScript, evType, outpoint, status, valueSat, roundID,
		batchExpiry, relativeExpiry, origin, commitmentTxid,
	)
	if err != nil {
		return err
	}

	if len(principals) == 0 {
		return nil
	}

	for _, mailboxID := range principals {
		msg := &indexerEventMessage{
			clientID: clientconn.ClientID(mailboxID),
			event:    incoming,
		}

		tellErr := o.cfg.Bridge.Tell(
			ctx, &clientconn.SendServerEventRequest{
				Message: msg,
			},
		)
		if tellErr != nil {
			o.log.Warnf("Indexer VTXO event tell failed for "+
				"client %q: %v", mailboxID, tellErr)
		}
	}

	return nil
}
