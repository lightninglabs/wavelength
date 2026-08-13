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
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const arkChannelMailboxRuntimePrefix = "arkchannel-serverconn-"

const arkChannelControllerPollInterval = 25 * time.Millisecond

const arkChannelCloseReceiveScriptLabel = "ark channel cooperative close"

// ArkChannelLifecycleController owns channel creation, inspection, and close.
type ArkChannelLifecycleController interface {
	PromoteVTXO(context.Context, btcutil.Amount) (arkchannel.Record, error)

	PromoteIncomingVHTLC(context.Context, lntypes.Hash, uint64,
		btcutil.Amount,
		ArkChannelClaimSource) (arkchannel.Record, error)

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

	WaitIncomingPayment(context.Context, lntypes.Hash) error
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

// ArkChannelClaimSource describes the exact vHTLC preimage input promoted by
// a receive fallback. It contains no signing authority.
type ArkChannelClaimSource struct {
	RecoveryID     string
	Outpoint       string
	Amount         btcutil.Amount
	PolicyTemplate []byte
	SpendPath      []byte
	PkScript       []byte
}

// ArkChannelClaimRootPreparer makes an externally described vHTLC available
// to the generic channel recovery archive without exposing it as wallet
// liquidity or starting unilateral recovery.
type ArkChannelClaimRootPreparer func(context.Context, arkchannel.Terms,
	ArkChannelClaimSource, arkchannel.ReceiveClaimRecoverySource,
	arkchannel.RecoveryPackage) error

// ArkChannelClaimOORPreparer prepares a channel OOR from an exact vHTLC claim
// input without crossing its signing point of no return.
type ArkChannelClaimOORPreparer func(context.Context, arkchannel.Terms,
	btcutil.Amount, ArkChannelClaimSource) (arkchannel.VTXOBinding, error)

// ArkChannelControllerConfig contains the process-owned dependencies supplied
// after wallet, database, and authenticated swap-server transport startup.
type ArkChannelControllerConfig struct {
	Log              btclog.Logger
	Store            *db.ArkChannelStoreDB
	Peer             lnruntime.ProcessCooperativeClosePeer
	PeerRPC          mailboxrpc.RPCClient
	PeerSender       lnruntime.PeerEventSender
	Wallet           *lwwallet.Wallet
	ChainBackend     chainsource.ChainBackend
	ChainNotifier    *chainbackends.BackendChainNotifier
	FeeEstimator     *chainfees.BackendEstimator
	OOR              *oorbridge.Controller
	Materializer     *unrollbridge.Controller
	Recovery         ArkChannelRecoveryController
	OperatorTerms    *types.OperatorTerms
	IdentityKey      keychain.KeyDescriptor
	OORDestination   *btcec.PublicKey
	KeyIndex         uint32
	NetParams        *chaincfg.Params
	ChannelDataDir   string
	PrepareOOR       ArkChannelOORPreparer
	PrepareClaimRoot ArkChannelClaimRootPreparer
	PrepareClaimOOR  ArkChannelClaimOORPreparer
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
	if !s.lwWallet.IsSome() {
		runtime.Stop()

		return fmt.Errorf("Ark channel runtime requires lwwallet")
	}
	if s.chainBackend == nil {
		runtime.Stop()

		return fmt.Errorf("Ark channel runtime requires a chain " +
			"backend")
	}
	if !s.unrollRegistryRef.IsSome() {
		runtime.Stop()

		return fmt.Errorf("Ark channel runtime requires the unroller")
	}
	chainNotifier, err := chainbackends.NewBackendChainNotifier(
		s.chainBackend,
	)
	if err != nil {
		runtime.Stop()

		return err
	}
	feeEstimator, err := chainfees.NewBackendEstimator(
		s.chainBackend, chainfee.FeePerKwFloor,
	)
	if err != nil {
		runtime.Stop()

		return err
	}
	oorController, err := oorbridge.New(s.actorSystem)
	if err != nil {
		runtime.Stop()

		return err
	}
	recovery, err := newArkChannelRecoveryArchive(
		s.vtxoStore, (&RPCServer{server: s}).newLocalOORArtifactStore(),
		s.chainBackend, s.subLogger(Subsystem),
	)
	if err != nil {
		runtime.Stop()

		return err
	}
	materializer, err := unrollbridge.NewController(
		s.unrollRegistryRef.UnsafeFromSome(), recovery,
	)
	if err != nil {
		runtime.Stop()

		return err
	}
	operatorTerms, err := s.fetchOperatorTerms(ctx)
	if err != nil {
		runtime.Stop()

		return fmt.Errorf("fetch Ark channel operator terms: %w", err)
	}
	controller, err := NewClientArkChannelController(
		ctx, ArkChannelControllerConfig{
			Log:            s.subLogger(Subsystem),
			Store:          s.arkChannelStore,
			Peer:           peer,
			PeerRPC:        runtime.Unary(),
			PeerSender:     peerSender,
			Wallet:         s.lwWallet.UnsafeFromSome(),
			ChainBackend:   s.chainBackend,
			ChainNotifier:  chainNotifier,
			FeeEstimator:   feeEstimator,
			OOR:            oorController,
			Materializer:   materializer,
			Recovery:       recovery,
			OperatorTerms:  operatorTerms,
			IdentityKey:    s.clientKeyDesc,
			OORDestination: s.clientKeyDesc.PubKey,
			NetParams:      s.chainParams,
			ChannelDataDir: filepath.Join(
				s.cfg.DataDir, "ark-channels",
			),
			PrepareOOR:       s.prepareArkChannelOOR,
			PrepareClaimRoot: s.prepareArkChannelClaimRoot,
			PrepareClaimOOR:  s.prepareArkChannelClaimOOR,
		},
	)
	if err != nil {
		runtime.Stop()

		return fmt.Errorf("construct Ark channel controller: %w", err)
	}
	if controller == nil {
		runtime.Stop()

		return fmt.Errorf("Ark channel controller factory returned nil")
	}
	connCfg.Dispatchers[lnruntime.PeerMessageRoute()] =
		lnruntime.NewPeerMessageDispatcher(
			controller.PeerMessageHandler(),
		)
	if err := runtime.StartIngress(ctx); err != nil {
		runtime.Stop()

		return fmt.Errorf("start Ark channel mailbox ingress: %w", err)
	}

	s.setArkChannelProcess(runtime, controller)

	return nil
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
	controller ArkChannelController) {

	s.arkChannelMu.Lock()
	defer s.arkChannelMu.Unlock()

	s.arkChannelMailboxRuntime = runtime
	s.arkChannelController = controller
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

// ArkChannelIncomingBackingFee returns the process-owned reserve used by
// receive fallback promotion. It is deliberately not a user-facing knob.
func (r *RPCServer) ArkChannelIncomingBackingFee() btcutil.Amount {
	return defaultArkChannelBackingFee
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
	hash lntypes.Hash) error {

	controller, err := r.waitArkChannelController(ctx)
	if err != nil {
		return err
	}

	return controller.WaitIncomingPayment(ctx, hash)
}

// PromoteArkChannelIncomingVHTLC negotiates backing before committing the
// preimage-path OOR claim into the channel-policy VTXO.
func (r *RPCServer) PromoteArkChannelIncomingVHTLC(ctx context.Context,
	hash lntypes.Hash, reservedSCID uint64, capacity btcutil.Amount,
	source ArkChannelClaimSource) (arkchannel.Record, error) {

	controller, err := r.waitArkChannelController(ctx)
	if err != nil {
		return arkchannel.Record{}, err
	}

	return controller.PromoteIncomingVHTLC(
		ctx, hash, reservedSCID, capacity, source,
	)
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
