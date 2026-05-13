package darepo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo/build"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightninglabs/darepo/metrics"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// RPCConfig contains configuration for the client-facing RPC server.
type RPCConfig struct {
	// ListenAddr is the network address the gRPC server binds to.
	ListenAddr string `mapstructure:"listen"`

	// TLS contains optional TLS certificate paths for the
	// client-facing gRPC server. When nil, the server runs
	// without TLS.
	TLS *TLSConfig `mapstructure:"tls"`

	// NoTLS explicitly disables TLS for the client-facing gRPC
	// server. Must be set when no TLS config is provided to
	// confirm the operator intends to run without transport
	// security.
	NoTLS bool `mapstructure:"notls"`

	// Listener is an optional pre-created listener. When non-nil,
	// the daemon serves on this listener instead of binding to
	// ListenAddr. This enables SDK-style embedding and in-memory
	// transports such as bufconn for tests.
	Listener net.Listener
}

// DefaultRPCConfig returns the default client RPC configuration.
func DefaultRPCConfig() *RPCConfig {
	return &RPCConfig{
		ListenAddr: DefaultRPCListen,
	}
}

// AdminRPCConfig contains configuration for the admin RPC server.
type AdminRPCConfig struct {
	// ListenAddr is the network address the admin gRPC server binds
	// to.
	ListenAddr string `mapstructure:"listen"`

	// Listener is an optional pre-created listener. When non-nil,
	// the daemon serves on this listener instead of binding to
	// ListenAddr. This enables SDK-style embedding and in-memory
	// transports such as bufconn for tests.
	Listener net.Listener
}

// DefaultAdminRPCConfig returns the default admin RPC configuration.
func DefaultAdminRPCConfig() *AdminRPCConfig {
	return &AdminRPCConfig{
		ListenAddr: DefaultAdminRPCListen,
	}
}

// RPCServer is a thin gRPC adapter that wraps the logical ark Server
// and serves client content over gRPC.
type RPCServer struct {
	// Required by the grpc-gateway/v2 library for forward compatibility.
	arkrpc.UnimplementedArkServiceServer

	cfg        *RPCConfig
	grpcServer *grpc.Server
	listener   net.Listener

	server *Server

	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	quit chan struct{}
	wg   sync.WaitGroup

	log btclog.Logger
}

// NewRPCServer creates a new client-facing RPC server.
func NewRPCServer(cfg *RPCConfig, operator *Server,
	log btclog.Logger) (*RPCServer, error) {

	// Use existing listener if provided, otherwise bind a new TCP
	// listener.
	listener := cfg.Listener
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", cfg.ListenAddr)
		if err != nil {
			return nil, fmt.Errorf("client RPC server unable to "+
				"listen on %s: %w", cfg.ListenAddr, err)
		}
	}

	// Build gRPC server options with mailbox auth interceptors
	// and metrics.
	requireTLS := cfg.TLS != nil
	grpcMetrics := metrics.GRPCServerMetrics

	serverOpts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(
			grpcMetrics.UnaryServerInterceptor(),
			newMailboxAuthInterceptor(log, requireTLS),
		),
		grpc.ChainStreamInterceptor(
			grpcMetrics.StreamServerInterceptor(),
			newMailboxStreamInterceptor(),
		),
	}

	// Wire TLS credentials when configured. The server requests
	// (but does not require) client certificates so the mTLS
	// interceptor can enforce per-RPC identity for clients that
	// present one.
	//
	// TODO(security): When TLS is not configured the server runs
	// without transport security. Do not deploy without TLS
	// outside regtest/dev environments.
	if cfg.TLS != nil {
		tlsCfg, err := loadServerTLSConfig(cfg.TLS)
		if err != nil {
			return nil, fmt.Errorf("load TLS config: %w", err)
		}

		serverOpts = append(
			serverOpts,
			grpc.Creds(
				credentials.NewTLS(tlsCfg),
			),
		)
	}

	s := &RPCServer{
		cfg:        cfg,
		server:     operator,
		log:        log,
		grpcServer: grpc.NewServer(serverOpts...),
		listener:   listener,
		quit:       make(chan struct{}),
	}

	// Register the client RPC service.
	arkrpc.RegisterArkServiceServer(s.grpcServer, s)

	return s, nil
}

// RegisterGRPCService allows co-hosting additional gRPC services on
// the client-facing server. The callback receives the underlying
// registrar so the caller can register arbitrary service
// implementations.
//
// Must be called before Start — panics if the server is already
// serving, since gRPC does not allow registration after Serve.
func (r *RPCServer) RegisterGRPCService(register func(grpc.ServiceRegistrar)) {
	if atomic.LoadUint32(&r.started) != 0 {
		panic("RegisterGRPCService called after Start")
	}

	register(r.grpcServer)
}

// Start starts the RPC server.
func (r *RPCServer) Start(ctx context.Context) error {
	if !atomic.CompareAndSwapUint32(&r.started, 0, 1) {
		return nil
	}

	r.log.InfoS(ctx, "Starting Client RPC server")

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		r.log.InfoS(ctx, "Client RPC server listening",
			"addr", r.listener.Addr(),
		)

		err := r.grpcServer.Serve(r.listener)
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			r.log.ErrorS(ctx, "Client RPC server exited with error",
				err,
			)
		}
	}()

	return nil
}

// Stop stops the RPC server.
func (r *RPCServer) Stop(ctx context.Context) error {
	if !atomic.CompareAndSwapUint32(&r.stopped, 0, 1) {
		return nil
	}

	r.log.InfoS(ctx, "Stopping client RPC server")

	close(r.quit)

	// Attempt a graceful shutdown so in-flight RPCs can
	// complete. Fall back to a hard stop after 5 seconds to
	// avoid blocking shutdown indefinitely.
	done := make(chan struct{})
	go func() {
		r.grpcServer.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		r.grpcServer.Stop()
	}

	r.wg.Wait()

	return nil
}

// Addr returns the address the RPC server is listening on.
func (r *RPCServer) Addr() net.Addr {
	if r.listener == nil {
		return nil
	}

	return r.listener.Addr()
}

// GetInfo returns basic information about the ark server.
func (r *RPCServer) GetInfo(ctx context.Context, req *arkrpc.GetInfoRequest) (
	*arkrpc.GetInfoResponse, error) {

	resp := &arkrpc.GetInfoResponse{
		Version: build.Version(),
		Network: r.server.cfg.Network,
	}

	if r.server.lnd != nil {
		_, height, err := r.server.lnd.ChainKit.GetBestBlock(
			ctx,
		)
		if err != nil {
			r.log.WarnS(ctx, "Unable to get best "+
				"block", err)
		} else {
			resp.BlockHeight = uint32(height)
		}
	}

	// Populate operator terms from the resolved batch terms.
	// These are set during rounds subsystem setup and required
	// by clients before they can create round actors.
	if t := r.server.terms; t != nil {
		// The pubkey field is the Ark operator key derived
		// from LND's wallet, not the LND node identity key.
		// Clients use this to construct boarding addresses
		// and validate round signatures.
		resp.Pubkey = t.OperatorKey.PubKey.
			SerializeCompressed()

		resp.SweepDelay = t.SweepDelay
		resp.BoardingExitDelay = t.BoardingExitDelay
		resp.VtxoExitDelay = t.VTXOExitDelay
		resp.DustLimit = int64(t.MinVTXOAmount)
		resp.MinBoardingAmount = int64(t.MinVTXOAmount)
		resp.MaxBoardingAmount = int64(t.MaxVTXOAmount)
		resp.MinConfirmations = t.MinBoardingConfirmations
		resp.MinOperatorFee = int64(t.MinOperatorFee)

		resp.SweepKey = t.SweepKey.PubKey.
			SerializeCompressed()
	}

	// Surface the operator's OOR lineage cap so clients can mirror
	// it in pre-submit cap arithmetic. Zero means the operator does
	// not enforce a cap.
	resp.MaxOorLineageVbytes = r.server.cfg.MaxOORLineageVBytes

	resp.ForfeitScript = r.server.forfeitScript

	// Include fee schedule parameters so clients can compute
	// fee estimates locally.
	if r.server.feeCalculator != nil {
		sched := r.server.feeCalculator.Schedule()
		resp.AnnualRate = sched.AnnualRate
		resp.BaseMarginSat = sched.BaseMarginSat
	}

	return resp, nil
}

// EstimateFee returns a fee breakdown for a given VTXO amount at
// current rates and utilization.
func (r *RPCServer) EstimateFee(ctx context.Context,
	req *arkrpc.EstimateFeeRequest) (*arkrpc.EstimateFeeResponse, error) {

	calc := r.server.feeCalculator
	if calc == nil {
		return nil, fmt.Errorf("fee calculator not configured")
	}

	// Get current fee rate.
	feeRate, err := r.server.feeEstimator.EstimateFeePerKW(
		r.server.cfg.Rounds.ConfTarget,
	)
	if err != nil {
		return nil, fmt.Errorf("estimate fee rate: %w", err)
	}

	// Get current utilization.
	utilization := 0.0
	if r.server.treasury != nil {
		utilization = r.server.treasury.Utilization()
	}

	// Quote sizes on-chain cost against batch=1 so every per-input
	// share is the maximum the server would ever charge. At seal
	// time the server recomputes at the actual registered count
	// (always >= 1), so a real per-client quote returns <= this
	// preview. The client treats this as an upper-bound estimate
	// only; the binding amount is the seal-time JoinRoundQuote.
	batchSize := 1

	// Compute a single effective remaining-blocks lifetime so that
	// the liquidity fee and min-viable calculations stay internally
	// consistent. When the caller omits remaining_blocks (or passes
	// zero) we fall back to the configured sweep delay, which is the
	// same horizon a freshly-forfeited VTXO would see.
	effectiveBlocks := req.RemainingBlocks
	if effectiveBlocks == 0 {
		effectiveBlocks = r.server.cfg.Rounds.SweepDelay
	}

	var breakdown *fees.FeeBreakdown
	if req.IsBoarding {
		breakdown = calc.ComputeBoardingFee(
			req.AmountSat, batchSize, feeRate,
		)
	} else {
		breakdown = calc.ComputeForfeitFee(
			req.AmountSat, batchSize, effectiveBlocks, feeRate,
			utilization,
		)
	}

	remainingDays := fees.BlocksToDays(effectiveBlocks)
	minViable := calc.MinViableAmount(
		batchSize, remainingDays, feeRate, utilization,
	)

	r.log.InfoS(ctx, "EstimateFee quote computed",
		slog.Int64("amount_sat", req.AmountSat),
		slog.Bool("is_boarding", req.IsBoarding),
		slog.Uint64(
			"conf_target", uint64(r.server.cfg.Rounds.ConfTarget),
		),
		slog.Int("batch_size", batchSize),
		slog.Int64("effective_blocks", int64(effectiveBlocks)),
		slog.Int64("fee_rate_sat_kw", int64(feeRate)),
		slog.Int64("fee_rate_sat_vbyte",
			int64(feeRate.FeePerVByte())),
		slog.Float64("utilization", utilization),
		slog.Int64("liquidity_fee_sat", breakdown.LiquidityFeeSat),
		slog.Int64("onchain_share_sat", breakdown.OnChainShareSat),
		slog.Int64("margin_sat", breakdown.MarginSat),
		slog.Int64("total_fee_sat", breakdown.TotalFeeSat),
		slog.Int64("min_viable_amount_sat", minViable),
		slog.Bool("below_min_viable", breakdown.BelowMinViable),
	)

	return &arkrpc.EstimateFeeResponse{
		LiquidityFeeSat:     breakdown.LiquidityFeeSat,
		OnchainShareSat:     breakdown.OnChainShareSat,
		MarginSat:           breakdown.MarginSat,
		TotalFeeSat:         breakdown.TotalFeeSat,
		EffectiveAnnualRate: breakdown.EffectiveAnnualRate,
		MinViableAmountSat:  minViable,
		BelowDustWarning:    breakdown.BelowMinViable,
	}, nil
}
