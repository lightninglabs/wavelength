package waved

import (
	"context"
	"fmt"
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
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const arkChannelControllerPollInterval = 25 * time.Millisecond

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
	SendPayment(context.Context, arkchannel.ID,
		btcutil.Amount) (ArkChannelPaymentResult, error)

	ReceivePayment(context.Context, arkchannel.ID,
		btcutil.Amount) (ArkChannelPaymentResult, error)

	PayLightningInvoice(context.Context, string,
		btcutil.Amount) (LightningPaymentResult, error)

	PrepareIncomingPayment(context.Context, lntypes.Preimage,
		btcutil.Amount) error

	RegisterIncomingPayment(context.Context, lntypes.Hash, btcutil.Amount,
		uint64) (uint32, error)

	WaitIncomingPayment(context.Context, lntypes.Hash) (arkchannel.ID, bool,
		error)

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
	Log            btclog.Logger
	Store          *db.ArkChannelStoreDB
	Peer           lnruntime.ProcessCooperativeClosePeer
	PeerRPC        mailboxrpc.RPCClient
	PeerSender     lnruntime.PeerEventSender
	Wallet         *lwwallet.Wallet
	ChainBackend   chainsource.ChainBackend
	ChainNotifier  *chainbackends.BackendChainNotifier
	FeeEstimator   *chainfees.BackendEstimator
	OOR            *oorbridge.Controller
	FundingOOR     arkchannel.OORTransferController
	Materializer   *unrollbridge.Controller
	Recovery       ArkChannelRecoveryController
	OperatorTerms  *types.OperatorTerms
	IdentityKey    keychain.KeyDescriptor
	OORDestination *btcec.PublicKey
	KeyIndex       uint32
	NetParams      *chaincfg.Params
	ChannelDataDir string

	// OpenChannelDB returns a database owned by this endpoint. Nil uses
	// the application SQL backend, including persistent SQLite in WASM.
	OpenChannelDB         func(string) (*channeldb.DB, error)
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
	controller := s.server.getArkChannelController()
	if controller == nil {
		return nil, status.Error(
			codes.Unavailable, "Ark channel runtime is not ready",
		)
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
	var result ArkChannelPaymentResult
	if receive {
		result, err = controller.ReceivePayment(ctx, id, amount)
	} else {
		result, err = controller.SendPayment(ctx, id, amount)
	}
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "channel "+
			"payment: %v", err)
	}

	return &arkchannelrpc.ChannelPaymentResponse{
		PaymentHash: result.PaymentHash[:], Settled: result.Settled,
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
		Channel: arkChannelRecordToRPC(ctx, controller, record),
	}, nil
}

// ListChannels returns every locally recoverable channel in stable store
// order, including pending creations whose initiating RPC timed out.
func (s *arkChannelRPCServer) ListChannels(ctx context.Context,
	_ *arkchannelrpc.ListChannelsRequest) (
	*arkchannelrpc.ListChannelsResponse, error) {

	controller := s.server.getArkChannelController()
	if controller == nil {
		return nil, status.Error(
			codes.Unavailable, "Ark channel runtime is not ready",
		)
	}
	records, err := controller.ListChannels(ctx)
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
	if record.Snapshot.Phase != arkchannel.PhaseActive {
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

// getArkChannelController returns the initialized local controller.
func (s *Server) getArkChannelController() ArkChannelController {
	s.arkChannelMu.RLock()
	defer s.arkChannelMu.RUnlock()

	return s.arkChannelController
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
