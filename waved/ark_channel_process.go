package waved

import (
	"context"
	"fmt"
	"math"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lnruntime"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightninglabs/wavelength/serverconn"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const arkChannelMailboxRuntimePrefix = "arkchannel-serverconn-"

// ArkChannelController is the local process boundary exposed through the
// daemon's ArkChannelService.
type ArkChannelController interface {
	RequestCooperativeClose(context.Context, arkchannel.ID,
		chainfee.SatPerKWeight) (arkchannel.Record, error)

	GetChannel(context.Context, arkchannel.ID) (arkchannel.Record, error)
}

// ArkChannelControllerConfig contains the process-owned dependencies supplied
// after wallet, database, and authenticated swap-server transport startup.
type ArkChannelControllerConfig struct {
	Store *db.ArkChannelStoreDB
	Peer  lnruntime.ProcessCooperativeClosePeer
}

// ArkChannelControllerFactory constructs the native channel controller. The
// factory closes over signer and lnd runtime dependencies owned by its process;
// RPC wiring receives only the durable store and authenticated peer edge.
type ArkChannelControllerFactory func(context.Context,
	ArkChannelControllerConfig) (ArkChannelController, error)

// arkChannelRPCServer exposes the configured controller on waved's existing
// authenticated local gRPC listener.
type arkChannelRPCServer struct {
	arkchannelrpc.UnimplementedArkChannelServiceServer

	server *Server
}

// RequestCooperativeClose starts or resumes the client-owned close process.
func (s *arkChannelRPCServer) RequestCooperativeClose(ctx context.Context,
	req *arkchannelrpc.RequestCooperativeCloseRequest) (
	*arkchannelrpc.RequestCooperativeCloseResponse, error) {

	id, err := arkChannelID(req.GetChannelId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if req.GetFeeRateSatPerKw() > math.MaxInt64 {
		return nil, status.Error(
			codes.InvalidArgument, "fee rate exceeds signed range",
		)
	}
	feeRate := chainfee.SatPerKWeight(req.GetFeeRateSatPerKw())
	if feeRate == 0 {
		feeRate = chainfee.FeePerKwFloor
	}
	if feeRate < chainfee.FeePerKwFloor {
		return nil, status.Errorf(codes.InvalidArgument, "fee rate is "+
			"below floor %d", chainfee.FeePerKwFloor)
	}
	controller := s.server.getArkChannelController()
	if controller == nil {
		return nil, status.Error(
			codes.Unavailable, "Ark channel runtime is not ready",
		)
	}
	record, err := controller.RequestCooperativeClose(ctx, id, feeRate)
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

// initArkChannelProcess builds a distinct swapdk-server mailbox runtime and
// then lets the process-owned factory compose native lnd and Ark dependencies.
func (s *Server) initArkChannelProcess(ctx context.Context) error {
	if s.cfg.ArkChannelControllerFactory == nil {
		return nil
	}
	if s.cfg.Swap == nil || s.cfg.Swap.ArkChannelMailbox == nil {
		return fmt.Errorf("Ark channel runtime requires the " +
			"authenticated swap-server mailbox")
	}
	if s.clientKeyDesc.PubKey == nil {
		return fmt.Errorf("Ark channel runtime requires client " +
			"identity")
	}
	if s.arkChannelStore == nil {
		return fmt.Errorf("Ark channel store is not initialized")
	}

	localMailbox := serverconn.PubKeyMailboxID(s.clientKeyDesc.PubKey)
	remoteMailbox := serverconn.CompoundMailboxID(
		"ark-channel-hub", localMailbox,
	)
	connCfg := serverconn.DefaultConnectorConfig()
	connCfg.RuntimeID = arkChannelMailboxRuntimePrefix + localMailbox
	connCfg.Edge = s.cfg.Swap.ArkChannelMailbox
	connCfg.LocalMailboxID = localMailbox
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
	if err := runtime.Start(ctx); err != nil {
		return fmt.Errorf("start Ark channel mailbox runtime: %w", err)
	}
	peer, err := lnruntime.NewMailboxCooperativeClosePeer(runtime.Unary())
	if err != nil {
		runtime.Stop()

		return err
	}
	controller, err := s.cfg.ArkChannelControllerFactory(
		ctx, ArkChannelControllerConfig{
			Store: s.arkChannelStore,
			Peer:  peer,
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

	s.setArkChannelProcess(runtime, controller)

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

var _ arkchannelrpc.ArkChannelServiceServer = (*arkChannelRPCServer)(nil)
