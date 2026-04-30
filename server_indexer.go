package darepo

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/indexer"
	"github.com/lightninglabs/darepo/mailbox"
	"github.com/lightninglabs/darepo/mailboxrpcserver"
	"github.com/lightninglabs/darepo/metrics"
	"github.com/lightninglabs/darepo/oor"
	"github.com/lightningnetwork/lnd/clock"
	"google.golang.org/grpc"
)

const (
	// defaultIndexerServerID is the operator identifier used by taproot
	// schnorr proof messages in the indexer flow.
	defaultIndexerServerID = "arkd"

	// defaultIndexerSenderMailboxID is the server identity stamped on
	// response and event envelopes sent by the indexer operator.
	defaultIndexerSenderMailboxID = "svc:indexer"

	// arkServiceName is the protobuf service name used in mailbox
	// envelope routing for the ArkService.
	arkServiceName = "arkrpc.ArkService"
)

// LocalMailboxClient adapts a MailboxServiceServer for in-process
// client usage.
//
// This avoids loopback gRPC networking when subsystems communicate
// through the shared mailbox store within the same process.
type LocalMailboxClient struct {
	server mailboxpb.MailboxServiceServer
}

// Send forwards the request to the in-process mailbox server.
func (c *LocalMailboxClient) Send(ctx context.Context,
	in *mailboxpb.SendRequest,
	_ ...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	return c.server.Send(ctx, in)
}

// Pull forwards the request to the in-process mailbox server.
func (c *LocalMailboxClient) Pull(ctx context.Context,
	in *mailboxpb.PullRequest,
	_ ...grpc.CallOption) (*mailboxpb.PullResponse, error) {

	return c.server.Pull(ctx, in)
}

// AckUpTo forwards the request to the in-process mailbox server.
func (c *LocalMailboxClient) AckUpTo(ctx context.Context,
	in *mailboxpb.AckUpToRequest,
	_ ...grpc.CallOption) (*mailboxpb.AckUpToResponse, error) {

	return c.server.AckUpTo(ctx, in)
}

// Compile-time interface check.
var _ mailboxpb.MailboxServiceClient = (*LocalMailboxClient)(nil)

// NewLocalMailboxClient builds an in-process mailbox client from the
// shared mailbox store.
func NewLocalMailboxClient(
	store mailbox.Store) (*LocalMailboxClient, error) {

	server, err := mailboxrpcserver.New(store)
	if err != nil {
		return nil, err
	}

	return &LocalMailboxClient{
		server: server,
	}, nil
}

// setupIndexerSubsystem initializes the indexer service, operator, and
// shared per-client bridge.
//
// The bridge hosts per-client runtimes that multiplex round and indexer RPC
// dispatchers. In the clientconn model, there is no standalone operator poll
// loop; instead each client's ingress loop pulls envelopes and dispatches
// them through the operator's DispatcherMap.
func (s *Server) setupIndexerSubsystem(ctx context.Context) error {
	// Create the durable mailbox store backing the local edge. All
	// subsystem envelope traffic (requests, responses, events) flows
	// through this store so cursors survive operator restarts.
	mailboxLog := subLogger(s.cfg.Loggers, mailboxSubsystem)
	mailboxOpts := s.cfg.mailboxStoreOptions()
	s.mailboxStore = db.NewMailboxEnvelopeStore(
		s.db, mailboxLog, mailboxOpts...,
	)

	edgeClient, err := NewLocalMailboxClient(s.mailboxStore)
	if err != nil {
		return fmt.Errorf("build local mailbox client: %w", err)
	}

	// Create the shared actor delivery store for per-client
	// runtimes created by auto-registration. This backs the
	// durable actor inbox and checkpoint persistence.
	clientDBStore := db.NewStore(
		s.db.DB(), s.db.Queries, s.db.Backend(),
		subLogger(s.cfg.Loggers, "CDBS"), nil,
	)
	s.deliveryStore, err = db.NewActorDeliveryStoreFromDB(
		clientDBStore, clock.NewDefaultClock(),
		subLogger(s.cfg.Loggers, "CDEL"),
	)
	if err != nil {
		return fmt.Errorf("create client delivery store: %w",
			err)
	}

	// Create the client status tracker so the bridge can report
	// per-client liveness derived from inbound envelope activity.
	s.statusTracker = clientconn.NewPullActivityTracker()

	// Register a callback to notify the metrics actor when clients
	// transition between online and offline. The metrics actor
	// handles all Prometheus gauge updates.
	s.statusTracker.OnStatusChange(
		func(_ clientconn.ClientID, status clientconn.ClientStatus) {
			var online bool
			switch status {
			case clientconn.StatusOnline:
				online = true
			case clientconn.StatusOffline:
				online = false
			default:
				return
			}

			s.tellMetrics(
				context.Background(),
				&metrics.ClientStatusChangedMsg{
					Online: online,
				},
			)
		},
	)

	// Create the shared per-client connection bridge. All subsystems
	// contribute dispatchers to this bridge so a single client
	// registration provides access to all server-side services.
	s.clientBridge = clientconn.NewClientsConnBridge(
		clientconn.WithOnUnknownClient(s),
		clientconn.WithStatusTracker(s.statusTracker),
	)

	// Create the indexer service with registration-based authorization.
	// Clients must register their receive scripts before querying for
	// events or VTXOs scoped to those scripts.
	indexerStore := indexer.NewSQLCStore(
		s.db.Queries,
		indexer.WithBatchedQuerier(s.db),
	)
	s.indexerService = indexer.NewService(
		defaultIndexerServerID, indexerStore,
	)
	s.indexerService.SetScriptAuthorizer(
		indexer.NewRegistrationScriptAuthorizer(indexerStore),
	)

	// Create the operator that provides RPC dispatchers and event
	// publication for the indexer service.
	s.indexerOperator, err = indexer.NewOperator(
		indexer.OperatorConfig{
			Edge:            edgeClient,
			SenderMailboxID: defaultIndexerSenderMailboxID,
			Bridge:          s.clientBridge,
		}, s.indexerService,
	)
	if err != nil {
		return fmt.Errorf("create indexer operator: %w", err)
	}

	s.log.InfoS(ctx, "Initialized indexer subsystem",
		"sender_mailbox_id", defaultIndexerSenderMailboxID)

	return nil
}

// stopIndexerSubsystem shuts down the shared bridge and releases indexer
// resources.
//
// In the clientconn model, the bridge owns all per-client runtimes. Stopping
// the bridge gracefully terminates every client's ingress loop, durable
// actor, and event router.
func (s *Server) stopIndexerSubsystem(ctx context.Context) {
	if s.clientBridge != nil {
		s.clientBridge.Stop()
	}

	if s.statusTracker != nil {
		s.statusTracker.Stop()
	}

	if s.indexerOperator != nil {
		s.log.InfoS(ctx, "Indexer subsystem stopped")
	}
}

// IndexerDispatchers returns the indexer operator's DispatcherMap for merging
// into per-client PerClientConfig.Dispatchers during client registration.
//
// Returns nil if the indexer subsystem has not been initialized.
func (s *Server) IndexerDispatchers() clientconn.DispatcherMap {
	if s.indexerOperator == nil {
		return nil
	}

	return s.indexerOperator.Dispatchers()
}

// ArkServiceDispatchers returns a DispatcherMap for ArkService
// methods (currently just GetInfo). These dispatchers route through
// the indexer operator's shared ServeMux, reusing its response
// envelope machinery.
//
// Returns nil if the indexer operator has not been initialized.
func (s *Server) ArkServiceDispatchers() clientconn.DispatcherMap {
	if s.indexerOperator == nil {
		return nil
	}

	return s.indexerOperator.ServiceDispatchers(
		arkServiceName, "GetInfo",
	)
}

// RegisterClientWithAllDispatchers creates a new per-client runtime
// with dispatchers merged from all active subsystems. This is the
// single entry point for client registration so callers do not need
// to know which subsystems are active.
//
// Dispatchers come from three sources:
//  1. Indexer operator (synchronous request-response via ServeMux,
//     covers both IndexerService and ArkService methods)
//  2. EventRouter (fire-and-forget routes for rounds and OOR RPCs)
//
// The full end-to-end flow for a fire-and-forget client request is:
//
//	Client → Mailbox → Ingress Loop → DispatcherMap lookup →
//	EnvelopeDispatcher → Unmarshal proto → Adapt(env, proto) →
//	actor.Tell (durable commit)
//
// For synchronous requests (indexer, ArkService) the flow goes
// through the operator's ServeMux and the response is sent back
// via Edge.Send.
//
// IMPORTANT: Each dispatcher is wrapped to overwrite env.Sender
// with the authenticated clientID before dispatch. The mailbox
// transport does not currently stamp Sender server-side, so
// env.Sender is client-controlled and untrusted. The wrapper
// ensures that all downstream code (rounds actor, indexer
// principal, etc.) receives the server-authenticated identity.
//
// TODO(security): Long-term, the mailbox transport layer should
// enforce Sender authenticity via mTLS or session-bound tokens
// so this wrapper becomes defense-in-depth rather than the
// primary trust boundary.
func (s *Server) RegisterClientWithAllDispatchers(ctx context.Context,
	clientID clientconn.ClientID,
	baseCfg clientconn.PerClientConfig) (*clientconn.ClientRuntime, error) {

	// Merge dispatchers from all active sources into the base
	// config. The indexer operator's Dispatchers covers
	// IndexerService methods; ArkServiceDispatchers covers
	// ArkService methods (GetInfo, etc.) via the same operator
	// mux. Rounds and OOR use the EventRouter for fire-and-forget
	// Tell dispatch.
	merged := make(clientconn.DispatcherMap)

	for k, v := range s.IndexerDispatchers() {
		merged[k] = v
	}

	for k, v := range s.ArkServiceDispatchers() {
		merged[k] = v
	}

	// Merge fire-and-forget routes from the shared EventRouter
	// (rounds + OOR RPCs).
	for k, v := range s.eventRouter.AsDispatcherMap() {
		merged[k] = v
	}

	// Register the heartbeat no-op dispatcher so the ingress loop
	// accepts heartbeat envelopes from the client. The real side
	// effect is the MarkActive call in the ingress loop after
	// successful dispatch.
	merged[clientconn.HeartbeatServiceMethod()] =
		clientconn.HeartbeatDispatcher()

	// Wrap every dispatcher to stamp the authenticated clientID
	// onto env.Sender before dispatch. This prevents a client
	// from spoofing another client's identity via the
	// client-controlled Sender field.
	authenticatedSender := string(clientID)
	for k, inner := range merged {
		dispatch := inner
		merged[k] = func(ctx context.Context,
			env *mailboxpb.Envelope) error {

			env.Sender = authenticatedSender

			return dispatch(ctx, env)
		}
	}

	baseCfg.Dispatchers = merged

	return s.clientBridge.RegisterClient(ctx, clientID, baseCfg)
}

// indexerRecipientNotifier bridges finalized OOR recipients into indexer
// EVENT emission without coupling OOR FSM state transitions to mailbox
// transport.
type indexerRecipientNotifier struct {
	operator *indexer.Operator
	log      btclog.Logger
}

// NotifyRecipientEvent best-effort publishes an incoming OOR mailbox EVENT.
func (n *indexerRecipientNotifier) NotifyRecipientEvent(
	ctx context.Context, sessionID oor.SessionID,
	recipient clientoor.ArkRecipientOutput) {

	if n == nil || n.operator == nil {
		return
	}

	n.log.InfoS(ctx, "Publishing OOR recipient notification",
		btclog.Hex("session_id", sessionID[:]),
		slog.Uint64("output_index", uint64(recipient.OutputIndex)),
		btclog.Hex("recipient_pk_script", recipient.PkScript),
		slog.Uint64("value_sat", uint64(recipient.Value)))

	sessionIDBytes := append([]byte(nil), sessionID[:]...)
	req := &arkrpc.OORRecipientEvent{
		RecipientPkScript: append(
			[]byte(nil), recipient.PkScript...,
		),
		SessionId:   sessionIDBytes,
		OutputIndex: recipient.OutputIndex,
		Value:       uint64(recipient.Value),
	}

	err := n.operator.PublishOORRecipientEvent(ctx, req)
	if err != nil {
		n.log.WarnS(ctx, "Failed to publish incoming OOR "+
			"indexer event", err)
	}
}

// Compile-time interface check.
var _ oor.RecipientNotifier = (*indexerRecipientNotifier)(nil)

// newOORRecipientNotifier builds the optional OOR->indexer notifier bridge.
//
// Returns nil if the indexer operator is not initialized.
func (s *Server) newOORRecipientNotifier() oor.RecipientNotifier {
	if s.indexerOperator == nil {
		return nil
	}

	return &indexerRecipientNotifier{
		operator: s.indexerOperator,
		log:      s.log,
	}
}

// indexerVTXONotifier bridges VTXO lifecycle transitions into indexer EVENT
// emission while keeping store mutations decoupled from mailbox transport.
type indexerVTXONotifier struct {
	operator *indexer.Operator
	log      btclog.Logger
}

// NotifyVTXOEvent best-effort publishes a VTXO lifecycle mailbox EVENT.
func (n *indexerVTXONotifier) NotifyVTXOEvent(ctx context.Context,
	event *db.VTXOEvent) {

	if n == nil || n.operator == nil || event == nil {
		return
	}

	if len(event.PkScript) == 0 {
		return
	}

	outpoint := event.Outpoint
	reqOutpoint := &arkrpc.OutPoint{
		Txid: outpoint.Hash[:],
		Vout: outpoint.Index,
	}

	eventType := mapVTXOEventTypeToRPC(event.Type)
	status := indexer.VTXOStatusFromStore(event.Status)

	err := n.operator.PublishVTXOEvent(
		ctx,
		append([]byte(nil), event.PkScript...),
		eventType,
		reqOutpoint,
		status,
		0, "", 0, 0,
		arkrpc.VTXOOrigin_VTXO_ORIGIN_UNSPECIFIED, nil,
	)
	if err != nil {
		n.log.WarnS(ctx, "Failed to publish VTXO indexer event",
			err)
	}
}

// mapVTXOEventTypeToRPC converts a DB-layer VTXO event type to the
// corresponding proto enum value.
func mapVTXOEventTypeToRPC(
	eventType db.VTXOEventType) arkrpc.VTXOEventType {

	switch eventType {
	case db.VTXOEventTypeCreated:
		return arkrpc.VTXOEventType_VTXO_EVENT_TYPE_CREATED

	case db.VTXOEventTypeStatusChanged:
		return arkrpc.VTXOEventType_VTXO_EVENT_TYPE_STATUS_CHANGED

	case db.VTXOEventTypeTerminated:
		return arkrpc.VTXOEventType_VTXO_EVENT_TYPE_TERMINATED

	default:
		return arkrpc.VTXOEventType_VTXO_EVENT_TYPE_UNSPECIFIED
	}
}

// newIndexerVTXONotifier builds the optional VTXO->indexer notifier bridge.
//
// Returns nil if the indexer operator is not initialized.
//
//nolint:unused
func (s *Server) newIndexerVTXONotifier() db.VTXOEventSink {
	if s.indexerOperator == nil {
		return nil
	}

	return &indexerVTXONotifier{
		operator: s.indexerOperator,
		log:      s.log,
	}
}

// Compile-time interface check.
var _ db.VTXOEventSink = (*indexerVTXONotifier)(nil)
