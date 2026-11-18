package waved

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/oorbridge"
	"github.com/lightninglabs/wavelength/arkchannel/unrollbridge"
	"github.com/lightninglabs/wavelength/chainbackends"
	"github.com/lightninglabs/wavelength/chainfees"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightninglabs/wavelength/lwwallet"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const arkChannelPeerIngressPrefix = "arkchannel-peer-ingress-"

const arkChannelControllerPollInterval = 25 * time.Millisecond

const arkChannelStartupRetryInterval = time.Second

const arkChannelRefreshReceiveScriptLabel = "ark channel refresh"

// ArkChannelLifecycleController owns channel creation, inspection, refresh,
// and on-chain close.
type ArkChannelLifecycleController interface {
	PromoteVTXO(context.Context, btcutil.Amount,
		string) (arkchannel.Record, error)

	MaterializeAndForceClose(context.Context, arkchannel.ID) (
		arkchannel.Record, chainhash.Hash, chainhash.Hash, error)

	RefreshChannel(context.Context,
		arkchannel.ID) (arkchannel.Record, error)

	GetChannel(context.Context, arkchannel.ID) (arkchannel.Record, error)

	ListChannels(context.Context) ([]arkchannel.Record, error)

	ChannelBalance(context.Context, arkchannel.ID) (btcutil.Amount,
		btcutil.Amount, error)

	PeerMessageHandler() lnruntime.PeerEventHandler

	Stop() error
}

// ArkChannelPaymentController owns the private/public payment bridge surface.
type ArkChannelPaymentController interface {
	PayLightningInvoice(context.Context, string,
		btcutil.Amount) (LightningPaymentResult, error)

	PrepareIncomingPayment(context.Context, lntypes.Preimage,
		btcutil.Amount) error

	RegisterIncomingPayment(context.Context, lntypes.Hash, btcutil.Amount,
		uint64) (arkchannel.ID, uint32, error)

	WaitIncomingPayment(context.Context,
		lntypes.Hash) (arkchannel.ID, error)

	SettleIncomingPayment(context.Context, lntypes.Preimage) error

	CancelIncomingPayment(context.Context, lntypes.Hash, string) error
}

// ArkChannelController is the complete local process boundary exposed through
// the daemon's ArkChannelService.
type ArkChannelController interface {
	ArkChannelLifecycleController
	ArkChannelPaymentController
}

// ArkChannelRecoveryController is the complete endpoint-local source archive,
// watcher, and unroll preparation boundary.
type ArkChannelRecoveryController interface {
	lnruntime.ChannelRecoveryManager
	arkchannel.ChannelEventSinkBinder
	unrollbridge.SourcePreparer

	RestoreWatches(context.Context, []arkchannel.Record) error

	SourceExpiring(context.Context, arkchannel.Terms,
		arkchannel.VTXOBinding, int32) (bool, error)

	Stop()
}

// ArkChannelOORPreparer reserves wallet VTXOs and prepares the exact OOR
// transfer that creates a channel-policy VTXO.
type ArkChannelOORPreparer func(context.Context, arkchannel.Terms,
	btcutil.Amount) (arkchannel.VTXOBinding, error)

// ArkChannelOORLookup reconciles a deterministic channel OOR key without
// selecting or locking fresh wallet inputs.
type ArkChannelOORLookup func(context.Context, arkchannel.Terms,
	btcutil.Amount) (oorbridge.PreparationLookup, error)

// ArkChannelControllerConfig contains the process-owned dependencies supplied
// after wallet, database, and authenticated swap-server transport startup.
type ArkChannelControllerConfig struct {
	Log           btclog.Logger
	Store         *db.ArkChannelStoreDB
	Peer          lnruntime.ProcessCooperativeClosePeer
	PeerRPC       mailboxrpc.RPCClient
	PeerSender    lnruntime.PeerEventSender
	Wallet        *lwwallet.Wallet
	ChainBackend  chainsource.ChainBackend
	ChainNotifier *chainbackends.BackendChainNotifier
	FeeEstimator  *chainfees.BackendEstimator
	OOR           *oorbridge.Controller
	Materializer  *unrollbridge.Controller
	Recovery      ArkChannelRecoveryController
	OperatorTerms *types.OperatorTerms
	// OperatorTermsSource reads the client's current negotiated policy.
	// Hub controllers can use the explicit OperatorTerms snapshot instead.
	OperatorTermsSource func() *types.OperatorTerms
	// RegisterRefreshPolicy registers this wallet's payout script under
	// current operator terms before those terms can be quoted for a close.
	RegisterRefreshPolicy func(context.Context) (
		*types.OperatorTerms, error)
	IdentityKey    keychain.KeyDescriptor
	OORDestination *btcec.PublicKey
	KeyIndex       uint32
	NetParams      *chaincfg.Params
	ChannelDataDir string

	// OpenChannelDB returns a database owned by this endpoint. Nil uses
	// the application SQL backend, including persistent SQLite in WASM.
	OpenChannelDB  func(string) (*channeldb.DB, error)
	PrepareOOR     ArkChannelOORPreparer
	LookupOOR      ArkChannelOORLookup
	Clock          clock.Clock
	RecordObserver arkchannel.RecordObserver
	// SourceExpiryTip returns the shared hub tip and next wakeup. A
	// standalone client subscribes directly to its own chain backend.
	SourceExpiryTip func() (int32, <-chan struct{})
}

// arkChannelRPCServer exposes the configured controller on waved's existing
// authenticated local gRPC listener.
type arkChannelRPCServer struct {
	arkchannelrpc.UnimplementedArkChannelServiceServer

	server *Server
}

// PromoteVTXO creates an OOR-backed native channel from existing wallet
// liquidity using daemon-derived protocol parameters.
func (s *arkChannelRPCServer) PromoteVTXO(ctx context.Context,
	req *arkchannelrpc.PromoteVTXORequest) (
	*arkchannelrpc.PromoteVTXOResponse, error) {

	amount, err := arkChannelAmount(req.GetAmountSat())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if req.GetIdempotencyKey() == "" {
		return nil, status.Error(
			codes.InvalidArgument, "idempotency key is required",
		)
	}
	if len(req.GetIdempotencyKey()) > maxArkChannelIdempotencyKeyLength {
		return nil, status.Errorf(codes.InvalidArgument, "idempotency "+
			"key exceeds %d bytes",
			maxArkChannelIdempotencyKeyLength)
	}
	controller, err := s.server.channelActionController(ctx, true)
	if err != nil {
		return nil, err
	}
	record, err := controller.PromoteVTXO(
		ctx, amount, req.GetIdempotencyKey(),
	)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		switch status.Code(err) {
		case codes.Aborted, codes.DeadlineExceeded,
			codes.ResourceExhausted, codes.Unavailable:
			return nil, status.Errorf(status.Code(err), "promote "+
				"VTXO: %v", err)

		default:
		}

		return nil, status.Errorf(codes.FailedPrecondition, "promote "+
			"VTXO: %v", err)
	}

	return &arkchannelrpc.PromoteVTXOResponse{
		Channel: arkChannelRecordToRPC(ctx, controller, record),
	}, nil
}

// PayLightningInvoice bridges a private source HTLC into the operator's
// public Lightning payment lifecycle with one shared payment hash.
func (s *arkChannelRPCServer) PayLightningInvoice(ctx context.Context,
	req *arkchannelrpc.PayLightningInvoiceRequest) (
	*arkchannelrpc.PayLightningInvoiceResponse, error) {

	if req.GetPaymentRequest() == "" {
		return nil, status.Error(
			codes.InvalidArgument, "payment request is required",
		)
	}
	if req.GetMaxFeeSat() > uint64(btcutil.MaxSatoshi) {
		return nil, status.Error(
			codes.InvalidArgument,
			"maximum fee exceeds maximum money",
		)
	}
	controller, err := s.server.channelActionController(ctx, false)
	if err != nil {
		return nil, err
	}
	result, err := controller.PayLightningInvoice(
		ctx, req.GetPaymentRequest(),
		btcutil.Amount(
			req.GetMaxFeeSat(),
		),
	)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "pay "+
			"Lightning invoice: %v", err)
	}

	return &arkchannelrpc.PayLightningInvoiceResponse{
		PaymentHash:      result.PaymentHash[:],
		Preimage:         result.Preimage[:],
		PrivateAmountSat: int64(result.PrivateAmount),
		FeeSat:           int64(result.Fee),
		ChannelId:        result.ChannelID[:],
	}, nil
}

// MaterializeAndForceClose publishes the Ark ancestry, signed backing, and
// latest native lnd commitment transaction.
func (s *arkChannelRPCServer) MaterializeAndForceClose(ctx context.Context,
	req *arkchannelrpc.MaterializeAndForceCloseRequest) (
	*arkchannelrpc.MaterializeAndForceCloseResponse, error) {

	id, err := arkChannelID(req.GetChannelId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	controller := s.server.getArkChannelController()
	if controller == nil {
		return nil, status.Error(
			codes.Unavailable, "Ark channel runtime is not ready",
		)
	}
	record, backingTxID, commitmentTxID, err :=
		controller.MaterializeAndForceClose(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"materialize and force close: %v", err)
	}

	return &arkchannelrpc.MaterializeAndForceCloseResponse{
		Channel:        lnruntime.ArkChannelRecordToRPC(record),
		BackingTxid:    backingTxID[:],
		CommitmentTxid: commitmentTxID[:],
	}, nil
}

// RefreshChannel starts or resumes the client-owned in-Ark refresh process.
func (s *arkChannelRPCServer) RefreshChannel(ctx context.Context,
	req *arkchannelrpc.RefreshChannelRequest) (
	*arkchannelrpc.RefreshChannelResponse, error) {

	id, err := arkChannelID(req.GetChannelId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	controller := s.server.getArkChannelController()
	if controller == nil {
		return nil, status.Error(
			codes.Unavailable, "Ark channel runtime is not ready",
		)
	}
	record, err := controller.RefreshChannel(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "refresh "+
			"channel in Ark: %v", err)
	}

	return &arkchannelrpc.RefreshChannelResponse{
		Channel: lnruntime.ArkChannelRecordToRPC(record),
	}, nil
}

// GetChannel returns one local durable Ark channel summary.
func (s *arkChannelRPCServer) GetChannel(ctx context.Context,
	req *arkchannelrpc.GetChannelRequest) (
	*arkchannelrpc.GetChannelResponse, error) {

	id, err := arkChannelID(req.GetChannelId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	controller := s.server.getArkChannelController()
	var record arkchannel.Record
	switch {
	case controller != nil:
		record, err = controller.GetChannel(ctx, id)

	case s.server.arkChannelStore != nil:
		record, err = s.server.arkChannelStore.Get(ctx, id)

	default:
		return nil, status.Error(
			codes.Unavailable, "Ark channel runtime is not ready",
		)
	}
	if errors.Is(err, arkchannel.ErrNotFound) {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get Ark channel: %v",
			err)
	}

	return &arkchannelrpc.GetChannelResponse{
		Channel: arkChannelRecordToRPC(ctx, controller, record),
	}, nil
}

// ListChannels returns every locally recoverable channel in stable store
// order, including pending creations whose initiating RPC timed out.
func (s *arkChannelRPCServer) ListChannels(ctx context.Context,
	_ *arkchannelrpc.ListChannelsRequest) (
	*arkchannelrpc.ListChannelsResponse, error) {

	controller := s.server.getArkChannelController()
	var records []arkchannel.Record
	var err error
	switch {
	case controller != nil:
		records, err = controller.ListChannels(ctx)

	case s.server.arkChannelStore != nil:
		records, err = s.server.arkChannelStore.ListNonTerminal(ctx)

	default:
		return nil, status.Error(
			codes.Unavailable, "Ark channel runtime is not ready",
		)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list Ark "+
			"channels: %v", err)
	}
	channels := make([]*arkchannelrpc.ArkChannel, 0, len(records))
	for _, record := range records {
		channels = append(
			channels,
			arkChannelRecordToRPC(ctx, controller, record),
		)
	}

	return &arkchannelrpc.ListChannelsResponse{Channels: channels}, nil
}

// arkChannelRecordToRPC appends live lnd balances when an active endpoint is
// available without hiding durable state when it is not.
func arkChannelRecordToRPC(ctx context.Context, controller ArkChannelController,
	record arkchannel.Record) *arkchannelrpc.ArkChannel {

	channel := lnruntime.ArkChannelRecordToRPC(record)
	if controller == nil ||
		record.Snapshot.Phase != arkchannel.PhaseActive {
		return channel
	}
	local, remote, err := controller.ChannelBalance(
		ctx, record.Snapshot.Terms.ID,
	)
	if err != nil {
		channel.BalanceError = err.Error()

		return channel
	}
	channel.LocalBalanceSat = int64(local)
	channel.RemoteBalanceSat = int64(remote)
	channel.BalanceAvailable = true

	return channel
}

// channelPeerTransport resolves the negotiated mailbox on each call. The
// native node can therefore restore chain protection before either remote
// service is reachable, without inventing a protocol version or accepting
// outgoing work into an unbound mailbox.
type channelPeerTransport struct {
	server *Server
}

// runtime returns only the fully attached transport.
func (p channelPeerTransport) runtime() (*serverconn.Runtime, error) {
	p.server.arkChannelMu.RLock()
	defer p.server.arkChannelMu.RUnlock()
	if p.server.arkChannelMailboxRuntime == nil {
		return nil, status.Error(
			codes.Unavailable, "Ark channel peer is offline",
		)
	}

	return p.server.arkChannelMailboxRuntime, nil
}

// SendRPC forwards a request only after the peer transport is online.
func (p channelPeerTransport) SendRPC(ctx context.Context,
	method mailboxrpc.ServiceMethod, req proto.Message,
	opts mailboxrpc.RPCOptions) (mailboxrpc.SendResult, error) {

	rt, err := p.runtime()
	if err != nil {
		return mailboxrpc.SendResult{}, err
	}

	return rt.Unary().SendRPC(ctx, method, req, opts)
}

// AwaitRPC waits on the same daemon-owned unary response store.
func (p channelPeerTransport) AwaitRPC(ctx context.Context, id string,
	resp proto.Message) error {

	rt, err := p.runtime()
	if err != nil {
		return err
	}

	return rt.Unary().AwaitRPC(ctx, id, resp)
}

// SendPeerEvent hands native traffic to the attached durable sender.
func (p channelPeerTransport) SendPeerEvent(ctx context.Context,
	event lnruntime.PeerEvent) error {

	rt, err := p.runtime()
	if err != nil {
		return err
	}
	sender, err := lnruntime.NewServerConnPeerSender(rt.TellRef())
	if err != nil {
		return err
	}

	return sender.SendPeerEvent(ctx, event)
}

// initLocalChannelController installs local chain recovery without a remote
// call. Startup and transport attachment have a single lifecycle owner.
func (s *Server) initLocalChannelController(ctx context.Context) error {
	if s.getArkChannelController() != nil {
		return nil
	}
	edge := channelPeerTransport{server: s}
	peer, err := lnruntime.NewMailboxCooperativeClosePeer(edge)
	if err != nil {
		return err
	}
	controller, err := s.newClientArkChannelController(
		ctx, s.loadClientKeyDesc(), peer, edge, edge,
	)
	if err != nil {
		return err
	}
	s.arkChannelMu.Lock()
	s.arkChannelController = controller
	s.arkChannelMu.Unlock()
	//nolint:contextcheck // controller owns its process-lifetime context
	controller.Start()

	return nil
}

// initArkChannelProcess attaches the client node to its negotiated transport.
func (s *Server) initArkChannelProcess(ctx context.Context) error {
	if s.cfg.Swap == nil || s.cfg.Swap.ArkChannelMailbox == nil {
		return nil
	}
	identityDesc := s.loadClientKeyDesc()
	if identityDesc.PubKey == nil {
		return fmt.Errorf("Ark channel runtime requires client " +
			"identity")
	}
	if s.arkChannelStore == nil {
		return fmt.Errorf("Ark channel store is not initialized")
	}
	if err := s.initLocalChannelController(ctx); err != nil {
		return err
	}
	controller := s.getArkChannelController()

	localMailbox := serverconn.PubKeyMailboxID(identityDesc.PubKey)
	connCfg := s.arkChannelConnectorConfig(localMailbox)

	//nolint:contextcheck // Start binds the runtime to the process context.
	runtime, err := serverconn.NewRuntime(connCfg)
	if err != nil {
		return fmt.Errorf("create Ark channel mailbox runtime: %w", err)
	}
	// The actor owns a process-lifetime context and is stopped by
	// Server.Stop.
	//nolint:contextcheck
	peerIngress, err := lnruntime.NewPeerMessageIngress(
		lnruntime.PeerMessageIngressConfig{
			ActorID: arkChannelPeerIngressPrefix + localMailbox,
			Store:   s.deliveryStore,
			Handler: controller.PeerMessageHandler(),
			Log:     s.subLogger(Subsystem),
		},
	)
	if err != nil {
		runtime.Stop()

		return fmt.Errorf("create Ark channel peer ingress: %w", err)
	}
	connCfg.Dispatchers[lnruntime.PeerMessageRoute()] =
		peerIngress.Dispatcher()
	// Start workers only after every peer route is installed. A retried
	// startup must not race an old egress worker on the same durable lane.
	runtime.StartEgress()
	if err := runtime.StartIngress(ctx); err != nil {
		joinCtx := context.WithoutCancel(ctx)

		return errors.Join(
			fmt.Errorf("start Ark channel mailbox ingress: %w",
				err),
			peerIngress.StopAndWait(joinCtx),
			runtime.StopAndWait(joinCtx),
		)
	}

	s.setArkChannelProcess(runtime, controller, peerIngress)

	return nil
}

// arkChannelConnectorConfig binds the dedicated durable lane to the same
// identity on every start, using the swap edge rather than the operator edge.
func (s *Server) arkChannelConnectorConfig(
	identity string) serverconn.ConnectorConfig {

	cfg := serverconn.DefaultConnectorConfig()
	cfg.Edge = s.cfg.Swap.ArkChannelMailbox
	cfg.LocalMailboxID = identity
	cfg.ReplyMailboxID = lnruntime.ArkChannelClientMailboxID(identity)
	cfg.RemoteMailboxID = lnruntime.ArkChannelHubMailboxID(identity)
	cfg.ArkProtocolVersion = s.arkProtocolVersion.Load()
	cfg.Store = s.deliveryStore
	cfg.Dispatchers = make(serverconn.DispatcherMap)
	cfg.Log = fn.Some(s.subLogger(serverconn.Subsystem))

	return cfg
}

// channelRecoveryRequired reads inventory even when the caller disables the
// runtime: unreadable state must never be treated as an empty wallet.
func channelRecoveryRequired(ctx context.Context, cfg *SwapConfig,
	list func(context.Context) ([]arkchannel.Record, error)) (bool, error) {

	records, err := list(ctx)
	if err != nil {
		return false, fmt.Errorf("read recoverable Ark channels: %w",
			err)
	}
	if len(records) == 0 {
		return false, nil
	}
	if cfg != nil && cfg.SuppressArkChannelRuntime {
		return false, fmt.Errorf("cannot disable Ark channel " +
			"recovery with recoverable channels")
	}

	return true, nil
}

// armArkChannelStartup creates no workers for a fresh wallet, even if it
// permits explicit operations. Only recovery or an explicit open consumes
// the one-shot starter. Its workers belong to the daemon, not an RPC.
func (s *Server) armArkChannelStartup(ctx context.Context) {
	if s.cfg.Swap == nil || s.cfg.Swap.SuppressArkChannelRuntime ||
		s.cfg.Swap.ArkChannelMailbox == nil {
		return
	}

	s.arkChannelMu.Lock()
	defer s.arkChannelMu.Unlock()
	s.arkChannelReady = make(chan struct{})
	s.arkChannelDone = ctx.Done()
	if s.arkChannelRecoveryNeeded {
		s.startArkChannelProcessSupervisor(ctx)

		return
	}
	s.arkChannelStart = func() {
		s.startArkChannelProcessSupervisor(ctx)
	}
}

// channelActionController checks admission independently of recovery.
// Paying an invoice never creates a channel or initializes an empty wallet.
func (s *Server) channelActionController(ctx context.Context, open bool) (
	ArkChannelController, error) {

	if s.cfg == nil || s.cfg.Swap == nil ||
		!s.cfg.Swap.ArkChannelsEnabled ||
		s.cfg.Swap.SuppressArkChannelRuntime {
		return nil, status.Error(
			codes.FailedPrecondition,
			"explicit Ark channel operations are disabled",
		)
	}
	s.arkChannelMu.Lock()
	if open && s.arkChannelStart != nil {
		s.arkChannelStart()
		s.arkChannelStart = nil
	}
	controller := s.arkChannelController
	if s.arkChannelMailboxRuntime == nil {
		controller = nil
	}
	ready, done := s.arkChannelReady, s.arkChannelDone
	s.arkChannelMu.Unlock()
	if controller == nil && open && ready != nil {
		select {
		case <-ready:
			controller = s.getArkChannelController()

		case <-done:
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	if controller == nil {
		return nil, status.Error(
			codes.Unavailable, "Ark channel runtime is not ready",
		)
	}

	return controller, nil
}

// startArkChannelProcessSupervisor starts the optional channel subsystem
// independently from ordinary wallet-ready services. A transient channel or
// indexer failure therefore cannot strand unrelated actors until restart.
func (s *Server) startArkChannelProcessSupervisor(ctx context.Context) {
	if s.cfg.Swap == nil || s.cfg.Swap.ArkChannelMailbox == nil {
		return
	}

	s.arkChannelStartupWg.Add(1)
	go func() {
		defer s.arkChannelStartupWg.Done()

		runRetriedArkChannelStartup(
			ctx, arkChannelStartupRetryInterval,
			s.initArkChannelProcess,
			func(err error) {
				s.log.WarnS(
					ctx,
					"Ark channel startup incomplete; "+
						"retrying",
					err,
				)
			},
		)
	}()
}

// runRetriedArkChannelStartup retries an idempotent channel startup function
// until it succeeds or the daemon context is canceled.
func runRetriedArkChannelStartup(ctx context.Context,
	retryInterval time.Duration, start func(context.Context) error,
	onFailure func(error)) {

	for {
		if err := start(ctx); err == nil {
			return
		} else if ctx.Err() != nil {
			return
		} else if onFailure != nil {
			onFailure(err)
		}

		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}

			return

		case <-timer.C:
		}
	}
}

// newClientArkChannelController composes the wallet, chain, OOR, and recovery
// dependencies behind the client channel controller.
func (s *Server) newClientArkChannelController(ctx context.Context,
	identityDesc keychain.KeyDescriptor,
	peer lnruntime.ProcessCooperativeClosePeer,
	peerRPC mailboxrpc.RPCClient, peerSender lnruntime.PeerEventSender) (
	*NativeArkChannelController, error) {

	if !s.lwWallet.IsSome() {
		return nil, fmt.Errorf("Ark channel runtime requires lwwallet")
	}
	if s.chainBackend == nil {
		return nil, fmt.Errorf("Ark channel runtime requires a chain " +
			"backend")
	}
	if !s.unrollRegistryRef.IsSome() {
		return nil, fmt.Errorf("Ark channel runtime requires the " +
			"unroller")
	}
	chainNotifier, err := chainbackends.NewBackendChainNotifier(
		s.chainBackend,
	)
	if err != nil {
		return nil, err
	}
	feeEstimator, err := chainfees.NewBackendEstimator(
		s.chainBackend, chainfee.FeePerKwFloor,
	)
	if err != nil {
		return nil, err
	}
	oorController, err := s.newArkChannelOORController()
	if err != nil {
		return nil, err
	}
	recovery, err := newArkChannelRecoveryArchive(
		s.vtxoStore, (&RPCServer{server: s}).newLocalOORArtifactStore(),
		s.chainBackend, s.subLogger(Subsystem),
	)
	if err != nil {
		return nil, err
	}
	materializer, err := unrollbridge.NewController(
		s.unrollRegistryRef.UnsafeFromSome(), recovery,
	)
	if err != nil {
		return nil, err
	}
	controller, err := NewClientArkChannelController(
		ctx, ArkChannelControllerConfig{
			Log:                   s.subLogger(Subsystem),
			Store:                 s.arkChannelStore,
			Peer:                  peer,
			PeerRPC:               peerRPC,
			PeerSender:            peerSender,
			Wallet:                s.lwWallet.UnsafeFromSome(),
			ChainBackend:          s.chainBackend,
			ChainNotifier:         chainNotifier,
			FeeEstimator:          feeEstimator,
			OOR:                   oorController,
			Materializer:          materializer,
			Recovery:              recovery,
			OperatorTermsSource:   s.loadOperatorTerms,
			RegisterRefreshPolicy: s.registerRefreshPolicy,
			IdentityKey:           identityDesc,
			OORDestination:        identityDesc.PubKey,
			NetParams:             s.chainParams,
			ChannelDataDir: filepath.Join(
				s.cfg.DataDir, "ark-channels",
			),
			PrepareOOR: s.prepareArkChannelOOR,
			LookupOOR:  s.lookupArkChannelOOR,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("construct Ark channel controller: %w",
			err)
	}
	if controller == nil {
		return nil, fmt.Errorf("Ark channel controller factory " +
			"returned nil")
	}

	return controller, nil
}

// registerRefreshPolicy returns exactly the operator policy used
// for registration, not the policy that originally funded the channel.
func (s *Server) registerRefreshPolicy(ctx context.Context) (
	*types.OperatorTerms, error) {

	operatorTerms, err := s.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch Ark channel refresh terms: %w",
			err)
	}

	if err := s.ensureArkChannelRefreshDelivery(
		ctx, operatorTerms,
	); err != nil {
		return nil, err
	}

	return operatorTerms, nil
}

// ensureArkChannelRefreshDelivery registers the durable identity-backed VTXO
// script before a refresh request can advertise it. The ordinary incoming OOR
// actor then owns materialization and fraud recovery for the replacement.
func (s *Server) ensureArkChannelRefreshDelivery(ctx context.Context,
	operatorTerms *types.OperatorTerms) error {

	if s.indexer == nil {
		return fmt.Errorf("Ark channel refresh requires the indexer")
	}
	if operatorTerms == nil || operatorTerms.PubKey == nil {
		return fmt.Errorf("Ark channel refresh requires operator terms")
	}
	store, err := (&RPCServer{server: s}).newOORReceiveScriptStore()
	if err != nil {
		return fmt.Errorf("initialize Ark channel refresh receive "+
			"store: %w", err)
	}
	signerFactory, err := s.indexerProofSignerFactory()
	if err != nil {
		return fmt.Errorf("initialize Ark channel refresh signer: %w",
			err)
	}
	identityDesc := s.loadClientKeyDesc()
	_, err = RegisterOwnedOORReceiveScript(
		ctx, s.indexer, store, identityDesc, signerFactory,
		operatorTerms.PubKey, operatorTerms.VTXOExitDelay,
		arkChannelRefreshReceiveScriptLabel,
	)
	if err != nil {
		return fmt.Errorf("register Ark channel refresh "+
			"destination: %w", err)
	}

	return nil
}

// setArkChannelProcess publishes the controller and its transport together.
func (s *Server) setArkChannelProcess(runtime *serverconn.Runtime,
	controller ArkChannelController,
	peerIngress *lnruntime.PeerMessageIngress) {

	s.arkChannelMu.Lock()
	defer s.arkChannelMu.Unlock()

	s.arkChannelMailboxRuntime = runtime
	s.arkChannelController = controller
	s.arkChannelPeerIngress = peerIngress
	if s.arkChannelReady != nil {
		close(s.arkChannelReady)
		s.arkChannelReady = nil
	}
}

// getArkChannelController returns the initialized local controller.
func (s *Server) getArkChannelController() ArkChannelController {
	s.arkChannelMu.RLock()
	defer s.arkChannelMu.RUnlock()

	return s.arkChannelController
}

// stopArkChannelProcess joins channel workers before the swap registrar
// releases their borrowed transport or the daemon closes the backing store.
func (s *Server) stopArkChannelProcess(ctx context.Context) error {
	s.arkChannelMu.Lock()
	runtime := s.arkChannelMailboxRuntime
	ingress := s.arkChannelPeerIngress
	controller := s.arkChannelController
	s.arkChannelMailboxRuntime = nil
	s.arkChannelPeerIngress = nil
	s.arkChannelController = nil
	s.arkChannelMu.Unlock()

	if runtime != nil {
		runtime.Connector().StopIngress()
	}
	err := ingress.StopAndWait(ctx)
	if controller != nil {
		err = errors.Join(err, controller.Stop())
	}
	if runtime != nil {
		err = errors.Join(err, runtime.StopAndWait(ctx))
	}

	return err
}

// PrepareArkChannelIncomingPayment installs the deterministic native invoice
// before a public route hint is exposed.
func (r *RPCServer) PrepareArkChannelIncomingPayment(ctx context.Context,
	preimage lntypes.Preimage, amount btcutil.Amount) error {

	controller, err := r.server.channelActionController(ctx, false)
	if err != nil {
		return err
	}

	return controller.PrepareIncomingPayment(ctx, preimage, amount)
}

// RegisterArkChannelIncomingPayment binds a public future SCID to the
// authenticated client endpoint after its private invoice is durable. It
// returns the minimum CLTV delta in blocks required by the private channel
// payment.
func (r *RPCServer) RegisterArkChannelIncomingPayment(ctx context.Context,
	hash lntypes.Hash, amount btcutil.Amount, reservedSCID uint64) (
	arkchannel.ID, uint32, error) {

	controller, err := r.server.channelActionController(ctx, false)
	if err != nil {
		return arkchannel.ID{}, 0, err
	}

	return controller.RegisterIncomingPayment(
		ctx, hash, amount, reservedSCID,
	)
}

// WaitArkChannelIncomingPayment waits on lnd's durable private invoice.
func (r *RPCServer) WaitArkChannelIncomingPayment(ctx context.Context,
	hash lntypes.Hash) (arkchannel.ID, error) {

	controller, err := r.waitArkChannelController(ctx)
	if err != nil {
		return arkchannel.ID{}, err
	}

	return controller.WaitIncomingPayment(ctx, hash)
}

// SettleArkChannelIncomingPayment releases the private hold invoice only after
// the swap SDK durably records acceptance on the registered channel.
func (r *RPCServer) SettleArkChannelIncomingPayment(ctx context.Context,
	preimage lntypes.Preimage) error {

	controller, err := r.waitArkChannelController(ctx)
	if err != nil {
		return err
	}

	return controller.SettleIncomingPayment(ctx, preimage)
}

// CancelArkChannelIncomingPayment cancels the registration and hold invoice.
func (r *RPCServer) CancelArkChannelIncomingPayment(ctx context.Context,
	hash lntypes.Hash, reason string) error {

	controller, err := r.waitArkChannelController(ctx)
	if err != nil {
		return err
	}

	return controller.CancelIncomingPayment(ctx, hash, reason)
}

// waitArkChannelController bridges optional subserver construction, which
// occurs before the mailbox-backed channel process is published at startup.
func (r *RPCServer) waitArkChannelController(ctx context.Context) (
	ArkChannelController, error) {

	if r == nil || r.server == nil {
		return nil, fmt.Errorf("Ark channel daemon is not initialized")
	}
	ticker := time.NewTicker(arkChannelControllerPollInterval)
	defer ticker.Stop()
	for {
		controller := r.server.getArkChannelController()
		if controller != nil {
			return controller, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()

		case <-ticker.C:
		}
	}
}

// arkChannelID parses one fixed-width durable channel identifier.
func arkChannelID(raw []byte) (arkchannel.ID, error) {
	var id arkchannel.ID
	if len(raw) != len(id) {
		return id, fmt.Errorf("channel id must be %d bytes, got %d",
			len(id), len(raw))
	}
	copy(id[:], raw)

	return id, nil
}

// arkChannelAmount parses a strictly positive channel amount.
func arkChannelAmount(value int64) (btcutil.Amount, error) {
	if value <= 0 {
		return 0, fmt.Errorf("amount must be positive")
	}

	return btcutil.Amount(value), nil
}

var _ arkchannelrpc.ArkChannelServiceServer = (*arkChannelRPCServer)(nil)
