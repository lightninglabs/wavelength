package waved

import (
	"context"
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
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const arkChannelMailboxRuntimePrefix = "arkchannel-serverconn-"

const arkChannelPeerIngressPrefix = "arkchannel-peer-ingress-"

const arkChannelControllerPollInterval = 25 * time.Millisecond

const arkChannelCloseReceiveScriptLabel = "ark channel cooperative close"

// ArkChannelLifecycleController owns channel creation, inspection, and close.
type ArkChannelLifecycleController interface {
	PromoteVTXO(context.Context, btcutil.Amount) (arkchannel.Record, error)

	MaterializeAndForceClose(context.Context, arkchannel.ID) (
		arkchannel.Record, chainhash.Hash, chainhash.Hash, error)

	RequestCooperativeClose(context.Context,
		arkchannel.ID) (arkchannel.Record, error)

	GetChannel(context.Context, arkchannel.ID) (arkchannel.Record, error)

	PeerMessageHandler() lnruntime.PeerEventHandler

	Stop() error
}

// ArkChannelPaymentController owns the private/public payment bridge surface.
type ArkChannelPaymentController interface {
	SendPayment(context.Context, arkchannel.ID,
		btcutil.Amount) (lntypes.Hash, error)

	ReceivePayment(context.Context, arkchannel.ID,
		btcutil.Amount) (lntypes.Hash, error)

	PayLightningInvoice(context.Context, string,
		btcutil.Amount) (LightningPaymentResult, error)

	PrepareIncomingPayment(context.Context, lntypes.Preimage,
		btcutil.Amount) error

	RegisterIncomingPayment(context.Context, lntypes.Hash, btcutil.Amount,
		uint64) error

	WaitIncomingPayment(context.Context,
		lntypes.Hash) (arkchannel.ID, error)
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

// ArkChannelReceiveCapitalReserver durably reserves global hub capital before
// an armed receive intent is allowed to select funding inputs.
type ArkChannelReceiveCapitalReserver func(context.Context,
	arkchannel.Terms) error

// ArkChannelControllerConfig contains the process-owned dependencies supplied
// after wallet, database, and authenticated swap-server transport startup.
type ArkChannelControllerConfig struct {
	Log                   btclog.Logger
	Store                 *db.ArkChannelStoreDB
	Peer                  lnruntime.ProcessCooperativeClosePeer
	PeerRPC               mailboxrpc.RPCClient
	PeerSender            lnruntime.PeerEventSender
	Wallet                *lwwallet.Wallet
	ChainBackend          chainsource.ChainBackend
	ChainNotifier         *chainbackends.BackendChainNotifier
	FeeEstimator          *chainfees.BackendEstimator
	OOR                   *oorbridge.Controller
	FundingOOR            arkchannel.OORTransferController
	Materializer          *unrollbridge.Controller
	Recovery              ArkChannelRecoveryController
	OperatorTerms         *types.OperatorTerms
	IdentityKey           keychain.KeyDescriptor
	OORDestination        *btcec.PublicKey
	KeyIndex              uint32
	NetParams             *chaincfg.Params
	ChannelDataDir        string
	PrepareOOR            ArkChannelOORPreparer
	LookupOOR             ArkChannelOORLookup
	ReserveReceiveCapital ArkChannelReceiveCapitalReserver
	Clock                 clock.Clock
	RecordObserver        arkchannel.RecordObserver
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
	controller := s.server.getArkChannelController()
	if controller == nil {
		return nil, status.Error(
			codes.Unavailable, "Ark channel runtime is not ready",
		)
	}
	record, err := controller.PromoteVTXO(ctx, amount)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "promote "+
			"VTXO: %v", err)
	}

	return &arkchannelrpc.PromoteVTXOResponse{
		Channel: lnruntime.ArkChannelRecordToRPC(record),
	}, nil
}

// SendPayment pays a hub invoice over one active native channel.
func (s *arkChannelRPCServer) SendPayment(ctx context.Context,
	req *arkchannelrpc.ChannelPaymentRequest) (
	*arkchannelrpc.ChannelPaymentResponse, error) {

	return s.channelPayment(ctx, req, false)
}

// ReceivePayment creates and settles a client invoice over one active native
// channel.
func (s *arkChannelRPCServer) ReceivePayment(ctx context.Context,
	req *arkchannelrpc.ChannelPaymentRequest) (
	*arkchannelrpc.ChannelPaymentResponse, error) {

	return s.channelPayment(ctx, req, true)
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
	controller := s.server.getArkChannelController()
	if controller == nil {
		return nil, status.Error(
			codes.Unavailable, "Ark channel runtime is not ready",
		)
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

// channelPayment validates a local payment request and dispatches it through
// the process-owned native channel controller.
func (s *arkChannelRPCServer) channelPayment(ctx context.Context,
	req *arkchannelrpc.ChannelPaymentRequest, receive bool) (
	*arkchannelrpc.ChannelPaymentResponse, error) {

	id, err := arkChannelID(req.GetChannelId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	amount, err := arkChannelAmount(req.GetAmountSat())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	controller := s.server.getArkChannelController()
	if controller == nil {
		return nil, status.Error(
			codes.Unavailable, "Ark channel runtime is not ready",
		)
	}
	var hash lntypes.Hash
	if receive {
		hash, err = controller.ReceivePayment(ctx, id, amount)
	} else {
		hash, err = controller.SendPayment(ctx, id, amount)
	}
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "channel "+
			"payment: %v", err)
	}

	return &arkchannelrpc.ChannelPaymentResponse{
		PaymentHash: hash[:], Settled: true,
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

// RequestCooperativeClose starts or resumes the client-owned close process.
func (s *arkChannelRPCServer) RequestCooperativeClose(ctx context.Context,
	req *arkchannelrpc.RequestCooperativeCloseRequest) (
	*arkchannelrpc.RequestCooperativeCloseResponse, error) {

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
	record, err := controller.RequestCooperativeClose(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "request "+
			"cooperative close: %v", err)
	}

	return &arkchannelrpc.RequestCooperativeCloseResponse{
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
	if controller == nil {
		return nil, status.Error(
			codes.Unavailable, "Ark channel runtime is not ready",
		)
	}
	record, err := controller.GetChannel(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "get Ark channel: %v",
			err)
	}

	return &arkchannelrpc.GetChannelResponse{
		Channel: lnruntime.ArkChannelRecordToRPC(record),
	}, nil
}

// initArkChannelProcess builds the client channel runtime whenever the swap
// runtime has installed its authenticated mailbox edge.
func (s *Server) initArkChannelProcess(ctx context.Context) error {
	if s.cfg.Swap == nil || s.cfg.Swap.ArkChannelMailbox == nil {
		return nil
	}
	if s.clientKeyDesc.PubKey == nil {
		return fmt.Errorf("Ark channel runtime requires client " +
			"identity")
	}
	if s.arkChannelStore == nil {
		return fmt.Errorf("Ark channel store is not initialized")
	}

	localMailbox := serverconn.PubKeyMailboxID(s.clientKeyDesc.PubKey)
	replyMailbox := lnruntime.ArkChannelClientMailboxID(localMailbox)
	remoteMailbox := lnruntime.ArkChannelHubMailboxID(localMailbox)
	connCfg := serverconn.DefaultConnectorConfig()
	connCfg.RuntimeID = arkChannelMailboxRuntimePrefix + localMailbox
	connCfg.Edge = s.cfg.Swap.ArkChannelMailbox
	connCfg.LocalMailboxID = localMailbox
	connCfg.ReplyMailboxID = replyMailbox
	connCfg.RemoteMailboxID = remoteMailbox
	connCfg.ArkProtocolVersion = s.arkProtocolVersion
	connCfg.Store = s.deliveryStore
	connCfg.Dispatchers = make(
		map[mailboxrpc.ServiceMethod]serverconn.EnvelopeDispatcher,
	)
	connCfg.Log = fn.Some(s.subLogger(serverconn.Subsystem))

	//nolint:contextcheck // Start binds the runtime to the process context.
	runtime, err := serverconn.NewRuntime(connCfg)
	if err != nil {
		return fmt.Errorf("create Ark channel mailbox runtime: %w", err)
	}
	// The mailbox runtime owns its worker context and cancels it in Stop.
	//nolint:contextcheck
	runtime.StartEgress()
	peer, err := lnruntime.NewMailboxCooperativeClosePeer(runtime.Unary())
	if err != nil {
		runtime.Stop()

		return err
	}
	peerSender, err := lnruntime.NewServerConnPeerSender(runtime.TellRef())
	if err != nil {
		runtime.Stop()

		return err
	}
	controller, err := s.newClientArkChannelController(
		ctx, peer, runtime.Unary(), peerSender,
	)
	if err != nil {
		runtime.Stop()

		return err
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
		_ = controller.Stop()
		runtime.Stop()

		return fmt.Errorf("create Ark channel peer ingress: %w", err)
	}
	connCfg.Dispatchers[lnruntime.PeerMessageRoute()] =
		peerIngress.Dispatcher()
	if err := runtime.StartIngress(ctx); err != nil {
		peerIngress.Stop()
		_ = controller.Stop()
		runtime.Stop()

		return fmt.Errorf("start Ark channel mailbox ingress: %w", err)
	}

	s.setArkChannelProcess(runtime, controller, peerIngress)

	return nil
}

// newClientArkChannelController composes the wallet, chain, OOR, and recovery
// dependencies behind the client channel controller.
func (s *Server) newClientArkChannelController(ctx context.Context,
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
	oorController, err := oorbridge.New(s.actorSystem)
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
			Log:            s.subLogger(Subsystem),
			Store:          s.arkChannelStore,
			Peer:           peer,
			PeerRPC:        peerRPC,
			PeerSender:     peerSender,
			Wallet:         s.lwWallet.UnsafeFromSome(),
			ChainBackend:   s.chainBackend,
			ChainNotifier:  chainNotifier,
			FeeEstimator:   feeEstimator,
			OOR:            oorController,
			Materializer:   materializer,
			Recovery:       recovery,
			IdentityKey:    s.clientKeyDesc,
			OORDestination: s.clientKeyDesc.PubKey,
			NetParams:      s.chainParams,
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

// ensureConfiguredArkChannelCloseDelivery registers the close destination
// once the main mailbox ingress is running. Registration is an indexer RPC and
// therefore cannot run while wallet-dependent actors are still being built.
func (s *Server) ensureConfiguredArkChannelCloseDelivery(
	ctx context.Context) error {

	if s.cfg.Swap == nil || s.cfg.Swap.ArkChannelMailbox == nil {
		return nil
	}
	operatorTerms, err := s.fetchOperatorTerms(ctx)
	if err != nil {
		return fmt.Errorf("fetch Ark channel close terms: %w", err)
	}

	return s.ensureArkChannelCloseDelivery(ctx, operatorTerms)
}

// ensureArkChannelCloseDelivery registers the durable identity-backed VTXO
// script before a close request can advertise it. The ordinary incoming OOR
// actor then owns materialization and fraud recovery for the replacement.
func (s *Server) ensureArkChannelCloseDelivery(ctx context.Context,
	operatorTerms *types.OperatorTerms) error {

	if s.indexer == nil {
		return fmt.Errorf("Ark channel close requires the indexer")
	}
	if operatorTerms == nil || operatorTerms.PubKey == nil {
		return fmt.Errorf("Ark channel close requires operator terms")
	}
	store, err := (&RPCServer{server: s}).newOORReceiveScriptStore()
	if err != nil {
		return fmt.Errorf("initialize Ark channel close receive "+
			"store: %w", err)
	}
	signerFactory, err := s.indexerProofSignerFactory()
	if err != nil {
		return fmt.Errorf("initialize Ark channel close signer: %w",
			err)
	}
	_, err = RegisterOwnedOORReceiveScript(
		ctx, s.indexer, store, s.clientKeyDesc, signerFactory,
		operatorTerms.PubKey, operatorTerms.VTXOExitDelay,
		arkChannelCloseReceiveScriptLabel,
	)
	if err != nil {
		return fmt.Errorf("register Ark channel close destination: %w",
			err)
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
}

// getArkChannelPeerIngress returns the durable BOLT ingress boundary.
func (s *Server) getArkChannelPeerIngress() *lnruntime.PeerMessageIngress {
	s.arkChannelMu.RLock()
	defer s.arkChannelMu.RUnlock()

	return s.arkChannelPeerIngress
}

// getArkChannelController returns the initialized local controller.
func (s *Server) getArkChannelController() ArkChannelController {
	s.arkChannelMu.RLock()
	defer s.arkChannelMu.RUnlock()

	return s.arkChannelController
}

// getArkChannelMailboxRuntime returns the initialized swap-server transport.
func (s *Server) getArkChannelMailboxRuntime() *serverconn.Runtime {
	s.arkChannelMu.RLock()
	defer s.arkChannelMu.RUnlock()

	return s.arkChannelMailboxRuntime
}

// PrepareArkChannelIncomingPayment installs the deterministic native invoice
// before a public route hint is exposed.
func (r *RPCServer) PrepareArkChannelIncomingPayment(ctx context.Context,
	preimage lntypes.Preimage, amount btcutil.Amount) error {

	controller, err := r.waitArkChannelController(ctx)
	if err != nil {
		return err
	}

	return controller.PrepareIncomingPayment(ctx, preimage, amount)
}

// RegisterArkChannelIncomingPayment binds a public future SCID to the
// authenticated client endpoint after its private invoice is durable.
func (r *RPCServer) RegisterArkChannelIncomingPayment(ctx context.Context,
	hash lntypes.Hash, amount btcutil.Amount, reservedSCID uint64) error {

	controller, err := r.waitArkChannelController(ctx)
	if err != nil {
		return err
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
