//go:build swapruntime

package swapclientserver

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/darepod"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	sdkark "github.com/lightninglabs/darepo-client/sdk/ark"
	"github.com/lightninglabs/darepo-client/sdk/swaps"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// swapClientService owns the daemon-side SwapClientService implementation.
//
// The service is intentionally a control-plane layer around sdk/swaps. It
// validates RPC requests, translates between protobuf and SDK summary models,
// starts one process-local background worker per payment hash, and publishes
// coarse summary updates to streaming subscribers. The durable swap state
// machine, store schema, timeout handling, and OOR claim/refund behavior remain
// delegated to sdk/swaps so the daemon RPC layer does not become a second swap
// implementation.
type swapClientService struct {
	swapclientrpc.UnimplementedSwapClientServiceServer

	// client is the narrow SDK-facing swap runtime used by all RPC
	// handlers and background workers. Production code supplies an adapter
	// around sdk/swaps.SwapClient; tests can supply a small fake that
	// exercises worker ownership without running real swap FSMs.
	client swapRuntimeClient

	// store is the daemon-owned durable swap store. The service keeps the
	// pointer so cleanup can close it with the same lifecycle as the
	// background workers that read from it.
	store *swaps.Store

	// log is the swapruntime subsystem logger derived from darepod's logger
	// manager. It is used only for daemon-owned orchestration events; the
	// SDK continues logging its own lower-layer details.
	log btclog.Logger

	// rootCtx is canceled when the daemon subserver shuts down. Workers use
	// this context instead of individual RPC contexts so CLI disconnects do
	// not cancel a swap that has already been admitted for background
	// execution.
	rootCtx context.Context //nolint:containedctx

	// cancel stops rootCtx and asks every active background worker to leave
	// its Wait call. The cleanup function returned from Register owns the
	// cancellation order.
	cancel context.CancelFunc

	// mu guards active and subscribers. Both maps are process-local runtime
	// coordination state and are intentionally not persisted; persisted
	// swap progress lives in sdk/swaps and is resumed through
	// resumePending.
	mu sync.Mutex

	// active is the set of payment-hash hex strings currently owned by a
	// background worker in this daemon process. It deduplicates concurrent
	// start/resume calls so a single payment hash is never driven by two
	// goroutines at once.
	active map[string]struct{}

	// subscribers is the set of live SubscribeSwaps streams. Each channel
	// is buffered and best-effort: workers never block on a slow
	// subscriber, and clients can recover current state with GetSwap or
	// ListSwaps.
	subscribers map[chan *swapclientrpc.SwapSummary]struct{}
}

// swapRuntimeClient is the narrow part of sdk/swaps that the daemon
// subserver needs in order to expose a background-control RPC layer. Keeping
// this seam local makes the subserver unit-testable without duplicating swap
// FSM behavior that still belongs to the SDK.
//
// Start methods persist newly requested swaps and return a session handle that
// identifies the payment hash. Resume methods rebuild a session handle from
// persisted state and are the only methods workers call before blocking in
// Wait. Summary methods expose durable state to RPC handlers without requiring
// the daemon layer to know the SDK store layout.
type swapRuntimeClient interface {
	// StartPayViaLightning creates a new pay swap for a Lightning invoice
	// and persists enough state for a background worker to resume it by
	// payment hash.
	StartPayViaLightning(context.Context, string,
		uint64) (paySwapSession, error)

	// StartReceiveViaLightning creates a new receive swap and returns the
	// invoice that callers hand to the remote payer.
	StartReceiveViaLightning(context.Context,
		btcutil.Amount) (receiveSwapSession, error)

	// ResumePayViaLightning reloads a persisted pay swap and returns the
	// FSM handle that the daemon worker should wait on.
	ResumePayViaLightning(context.Context,
		lntypes.Hash) (paySwapSession, error)

	// ResumeReceiveViaLightning reloads a persisted receive swap and
	// returns the FSM handle that the daemon worker should wait on.
	ResumeReceiveViaLightning(context.Context,
		lntypes.Hash) (receiveSwapSession, error)

	// GetSwapSummary reads one durable pay or receive swap summary by its
	// Lightning payment hash.
	GetSwapSummary(context.Context, lntypes.Hash) (swaps.SwapSummary, error)

	// ListSwapSummaries reads durable swap summaries, optionally filtering
	// to swaps that sdk/swaps still considers pending.
	ListSwapSummaries(context.Context, bool) ([]swaps.SwapSummary, error)
}

// paySwapSession is the subset of a pay swap FSM that the daemon executor
// drives. The real implementation is sdk/swaps.PaySession; tests provide a
// small blocking fake so worker ownership can be asserted deterministically.
type paySwapSession interface {
	// PaymentHash returns the durable identifier used to deduplicate
	// workers and address the swap through the public RPC service.
	PaymentHash() lntypes.Hash

	// Wait drives or observes the pay swap FSM until it reaches terminal
	// state or the supplied daemon-root context is canceled.
	Wait(context.Context) (*swaps.PayResult, error)
}

// receiveSwapSession is the subset of a receive swap FSM that the daemon
// executor drives. The accessor methods make the start response independent
// from sdk/swaps.ReceiveSession's exported fields, which keeps tests simple.
type receiveSwapSession interface {
	// PaymentHash returns the durable identifier used to deduplicate
	// workers and address the swap through the public RPC service.
	PaymentHash() lntypes.Hash

	// Invoice returns the BOLT-11 invoice created by sdk/swaps for the
	// receive swap start response.
	Invoice() string

	// Wait drives or observes the receive swap FSM until it reaches
	// terminal state or the supplied daemon-root context is canceled.
	Wait(context.Context) (*swaps.ReceiveResult, error)
}

// swapClientAdapter adapts sdk/swaps.SwapClient to the narrow
// swapRuntimeClient interface used by the subserver. It is intentionally thin:
// all state transitions, persistence, timeout handling, and claim/refund logic
// remain inside sdk/swaps.
type swapClientAdapter struct {
	// client is the concrete SDK swap client. The adapter does not own the
	// client's transports or store; the Register cleanup path owns those
	// resources.
	client *swaps.SwapClient
}

// receiveSessionAdapter adds method accessors around sdk/swaps.ReceiveSession's
// public start-response fields so both production code and tests share the
// same receiveSwapSession interface.
type receiveSessionAdapter struct {
	// session is the concrete SDK receive session returned by a new or
	// resumed receive swap.
	session *swaps.ReceiveSession
}

// Register installs the optional SwapClientService on the daemon gRPC server.
//
// The function is called only from a swapruntime-tagged darepod binary. It
// opens the daemon-owned swap store, constructs the sdk/swaps client, registers
// the separate swapclientrpc subserver on the existing daemon listener, and
// resumes all persisted pending swap sessions before returning. The returned
// cleanup function stops background workers and closes the swap store and
// swapdk-server connection during daemon shutdown.
//
// When cfg.Swap.SuppressResume is true, the synchronous resumePending sweep
// is skipped so a higher layer (the walletrpc subserver) can own the unified
// resume policy. In that case cfg.Swap.Backend is populated so the higher
// layer can drive ResumePending itself after performing any cross-subsystem
// preconditions.
func Register(ctx context.Context, grpcServer *grpc.Server,
	rpcServer *darepod.RPCServer, cfg *darepod.Config) (func(), error) {

	svc, cleanup, err := newSwapClientService(ctx, rpcServer, cfg)
	if err != nil {
		return nil, err
	}

	swapclientrpc.RegisterSwapClientServiceServer(grpcServer, svc)

	// Publish the backend handle so the walletrpc registrar (if compiled
	// in) can drive ResumePending and any future in-Go calls without
	// going through the gRPC stub. Done before the resume sweep so the
	// handle is reachable even when this layer is configured to skip its
	// own sweep.
	if cfg.Swap != nil {
		cfg.Swap.Backend = svc
	}

	suppressResume := cfg.Swap != nil && cfg.Swap.SuppressResume
	if !suppressResume {
		svc.resumePending(ctx)
	}

	return cleanup, nil
}

// ResumePending re-arms background workers for every persisted pending swap
// session. It is idempotent: payment hashes already owned by an active worker
// are skipped. The method satisfies darepod.SwapBackend so a higher subserver
// (such as walletrpc) can drive the resume sweep as part of a unified
// wallet-level lifecycle policy.
func (s *swapClientService) ResumePending(ctx context.Context) {
	s.resumePending(ctx)
}

// newSwapClientService builds the daemon-owned swap executor from darepod
// runtime dependencies.
//
// The constructor opens the daemon swap store, dials swapdk-server, creates an
// in-process Ark SDK facade over darepod's existing DaemonService, and wires
// the sdk/swaps client. Receive-auth signing and ECDH are delegated back to the
// daemon through the Ark SDK facade, so the swapruntime layer does not persist
// its own receive-auth key material. It also returns a cleanup function that
// must be called during daemon shutdown so the root worker context is canceled
// before the Ark, swapdk-server, and store resources are closed.
func newSwapClientService(ctx context.Context, rpcServer *darepod.RPCServer,
	daemonCfg *darepod.Config) (*swapClientService, func(), error) {

	cfg := daemonCfg.Swap
	if cfg == nil {
		cfg = &darepod.SwapConfig{}
	}

	dbPath := cfg.DatabaseFileName
	if dbPath == "" {
		dbPath = filepath.Join(daemonCfg.DataDir, "swaps.db")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create swap db dir: %w", err)
	}

	log := btclog.Disabled
	if rpcServer != nil {
		log = rpcServer.SubLogger(darepod.SwapSubsystem)
	}

	store, err := swaps.NewSqliteStore(&swaps.SqliteStoreConfig{
		DatabaseFileName: dbPath,
	}, log)
	if err != nil {
		return nil, nil, fmt.Errorf("open swap store: %w", err)
	}

	swapAddr := cfg.ServerAddress
	if swapAddr == "" {
		swapAddr = "localhost:10030"
	}

	dialOpts, err := swapServerDialOptions(cfg, swapAddr)
	if err != nil {
		_ = store.Close()

		return nil, nil, err
	}

	swapConn, err := grpc.NewClient(swapAddr, dialOpts...)
	if err != nil {
		_ = store.Close()

		return nil, nil, fmt.Errorf("connect to swap server: %w", err)
	}

	serverConn := swaps.NewGRPCSwapServerConn(swapConn)
	arkClient, err := sdkark.WrapDaemonServer(ctx, sdkark.InProcessConfig{
		DaemonServer: rpcServer,
	})
	if err != nil {
		_ = serverConn.Close()
		_ = swapConn.Close()
		_ = store.Close()

		return nil, nil, fmt.Errorf("create in-process ark client: %w",
			err)
	}

	chainParams, err := chainParamsForNetwork(daemonCfg.Network)
	if err != nil {
		_ = arkClient.Close()
		_ = serverConn.Close()
		_ = swapConn.Close()
		_ = store.Close()

		return nil, nil, err
	}

	invoiceGen, err := daemonInvoiceGenerator(arkClient, chainParams)
	if err != nil {
		_ = arkClient.Close()
		_ = serverConn.Close()
		_ = swapConn.Close()
		_ = store.Close()

		return nil, nil, err
	}

	rootCtx, cancel := context.WithCancel(ctx)
	swapClient := swaps.NewSwapClientWithStore(
		serverConn, arkClient, log, invoiceGen, store,
	)
	swapClient.SetOutSwapEventReceiver(
		swaps.NewMailboxOutSwapEventReceiver(
			// Empty mailbox ID makes the receiver derive the
			// per-swap mailbox from the client identity key and
			// payment hash.
			mailboxpb.NewMailboxServiceClient(swapConn), "",
		),
	)

	service := &swapClientService{
		client: &swapClientAdapter{
			client: swapClient,
		},
		store:       store,
		log:         log,
		rootCtx:     rootCtx,
		cancel:      cancel,
		active:      make(map[string]struct{}),
		subscribers: make(map[chan *swapclientrpc.SwapSummary]struct{}),
	}

	cleanup := func() {
		cancel()
		_ = arkClient.Close()
		_ = serverConn.Close()
		_ = swapConn.Close()
		_ = store.Close()
	}

	return service, cleanup, nil
}

// StartPayViaLightning starts a real sdk/swaps pay session and returns it
// through the narrow paySwapSession interface expected by the daemon service.
func (a *swapClientAdapter) StartPayViaLightning(ctx context.Context,
	invoice string, maxFeeSat uint64) (paySwapSession, error) {

	return a.client.StartPayViaLightning(ctx, invoice, maxFeeSat)
}

// StartReceiveViaLightning starts a real sdk/swaps receive session and wraps
// it with method accessors for the daemon RPC response path.
func (a *swapClientAdapter) StartReceiveViaLightning(ctx context.Context,
	amountSat btcutil.Amount) (receiveSwapSession, error) {

	session, err := a.client.StartReceiveViaLightning(ctx, amountSat)
	if err != nil {
		return nil, err
	}

	return &receiveSessionAdapter{session: session}, nil
}

// ResumePayViaLightning reloads a persisted pay session from sdk/swaps and
// returns it through the daemon worker interface.
func (a *swapClientAdapter) ResumePayViaLightning(ctx context.Context,
	hash lntypes.Hash) (paySwapSession, error) {

	return a.client.ResumePayViaLightning(ctx, hash)
}

// ResumeReceiveViaLightning reloads a persisted receive session from sdk/swaps
// and wraps it with method accessors for the daemon worker interface.
func (a *swapClientAdapter) ResumeReceiveViaLightning(ctx context.Context,
	hash lntypes.Hash) (receiveSwapSession, error) {

	session, err := a.client.ResumeReceiveViaLightning(ctx, hash)
	if err != nil {
		return nil, err
	}

	return &receiveSessionAdapter{session: session}, nil
}

// GetSwapSummary forwards direct payment-hash lookups to sdk/swaps so the
// daemon RPC layer does not scan every persisted session on hot paths.
func (a *swapClientAdapter) GetSwapSummary(ctx context.Context,
	hash lntypes.Hash) (swaps.SwapSummary, error) {

	return a.client.GetSwapSummary(ctx, hash)
}

// ListSwapSummaries forwards summary reads to sdk/swaps without applying any
// daemon-side state interpretation.
func (a *swapClientAdapter) ListSwapSummaries(ctx context.Context,
	pendingOnly bool) ([]swaps.SwapSummary, error) {

	return a.client.ListSwapSummaries(ctx, pendingOnly)
}

// PaymentHash returns the Lightning payment hash associated with the receive
// session returned by sdk/swaps.
func (r *receiveSessionAdapter) PaymentHash() lntypes.Hash {
	return r.session.PaymentHash
}

// Invoice returns the BOLT-11 invoice produced by sdk/swaps for a receive
// session.
func (r *receiveSessionAdapter) Invoice() string {
	return r.session.Invoice
}

// Wait blocks until the wrapped receive session reaches a terminal state or
// the caller cancels the provided context.
func (r *receiveSessionAdapter) Wait(ctx context.Context) (*swaps.ReceiveResult,
	error) {

	return r.session.Wait(ctx)
}

// daemonInvoiceGenerator constructs the invoice creator used by the
// daemon-owned swap client.
//
// All production receive flows use CreateInvoiceWithKey with a payment-scoped
// auth key derived inside the daemon (see PR #337): sdk/swaps overrides the
// invoice NodeSigner with the daemon-supplied auth key at signing time, so the
// underlying generator's backing key is never invoked to sign a user-visible
// invoice. To make this property assertion-backed, the returned creator wraps
// the underlying generator in daemonAuthOnlyInvoiceCreator, which rejects the
// no-key CreateInvoice path entirely. If a future code change ever takes that
// path inside the daemon, callers will see an explicit error instead of
// silently signing with the ephemeral plumbing key.
func daemonInvoiceGenerator(arkClient *sdkark.Client,
	chainParams *chaincfg.Params) (swaps.InvoiceCreator, error) {

	// The ephemeral key here is plumbing required by
	// NewEphemeralInvoiceGenerator's signature; it is never used to sign
	// anything in the daemon path because daemonAuthOnlyInvoiceCreator
	// forbids the no-key CreateInvoice path and CreateInvoiceWithKey
	// overrides the NodeSigner with the daemon-derived auth key.
	plumbingKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("create plumbing invoice key: %w", err)
	}

	bestHeight := func() (uint32, error) {
		info, err := arkClient.GetInfo(context.Background())
		if err != nil {
			return 0, err
		}

		return info.BlockHeight, nil
	}

	return &daemonAuthOnlyInvoiceCreator{
		inner: swaps.NewEphemeralInvoiceGenerator(
			plumbingKey, bestHeight, chainParams,
		),
	}, nil
}

// daemonAuthOnlyInvoiceCreator enforces that all invoice creation in the
// daemon swap path goes through CreateInvoiceWithKey with an explicit
// daemon-derived auth key. The no-key CreateInvoice path is rejected so that
// a regression cannot accidentally produce an invoice signed by ephemeral
// plumbing state instead of a payment-scoped daemon key.
type daemonAuthOnlyInvoiceCreator struct {
	inner swaps.InvoiceCreator
}

// CreateInvoice rejects the no-key invoice creation path. In the daemon swap
// flow every invoice must be signed with a payment-scoped auth key returned
// by the wallet backend, so direct CreateInvoice is a programming error.
func (d *daemonAuthOnlyInvoiceCreator) CreateInvoice(_ context.Context,
	_ btcutil.Amount, _ string, _ *swaps.RouteHint, _ time.Duration,
	_ *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash, error) {

	return nil, lntypes.Hash{}, fmt.Errorf("daemon swap path requires " +
		"CreateInvoiceWithKey with a daemon-derived auth key")
}

// CreateInvoiceWithKey delegates to the underlying generator, which overrides
// the NodeSigner with the supplied authKey so the daemon-managed key signs
// the invoice rather than the generator's plumbing key.
func (d *daemonAuthOnlyInvoiceCreator) CreateInvoiceWithKey(
	ctx context.Context, amountSat btcutil.Amount, memo string,
	routeHint *swaps.RouteHint, expiry time.Duration,
	authKey keychain.SingleKeyMessageSigner,
	preimage *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash, error) {

	return d.inner.CreateInvoiceWithKey(
		ctx, amountSat, memo, routeHint, expiry, authKey, preimage,
	)
}

// swapServerDialOptions maps daemon swap config into gRPC transport options
// for swapdk-server. Loopback and unix-socket endpoints default to insecure
// transport for regtest ergonomics; non-local endpoints use TLS unless the
// caller explicitly provides ServerInsecure.
func swapServerDialOptions(cfg *darepod.SwapConfig,
	addr string) ([]grpc.DialOption, error) {

	switch {
	case cfg.ServerTLSCertPath != "":
		creds, err := credentials.NewClientTLSFromFile(
			cfg.ServerTLSCertPath, "",
		)
		if err != nil {
			return nil, fmt.Errorf("load swap server TLS "+
				"certificate: %w", err)
		}

		return []grpc.DialOption{
			grpc.WithTransportCredentials(creds),
		}, nil

	case isLocalSwapServerAddr(addr) || cfg.ServerInsecure:
		return []grpc.DialOption{
			grpc.WithTransportCredentials(
				insecure.NewCredentials(),
			),
		}, nil

	default:
		return []grpc.DialOption{
			grpc.WithTransportCredentials(
				credentials.NewTLS(
					&tls.Config{
						MinVersion: tls.VersionTLS12,
					},
				),
			),
		}, nil
	}
}

// chainParamsForNetwork converts the daemon's configured network string into
// the btcd chain parameters required by the invoice generator.
func chainParamsForNetwork(network string) (*chaincfg.Params, error) {
	switch network {
	case "mainnet", "bitcoin":
		return &chaincfg.MainNetParams, nil

	case "testnet", "testnet3":
		return &chaincfg.TestNet3Params, nil

	case "regtest":
		return &chaincfg.RegressionNetParams, nil

	case "simnet":
		return &chaincfg.SimNetParams, nil

	case "signet":
		return &chaincfg.SigNetParams, nil

	default:
		return nil, fmt.Errorf("unknown network %q", network)
	}
}

// isLocalSwapServerAddr reports whether a configured swapdk-server address is
// scoped to the local machine. Local endpoints are treated as development
// endpoints and may be dialed with insecure gRPC credentials by default.
func isLocalSwapServerAddr(addr string) bool {
	if strings.HasPrefix(addr, "unix:") {
		return true
	}

	host := addr
	splitHost, _, err := net.SplitHostPort(addr)
	if err == nil {
		host = splitHost
	}

	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// StartPay persists a pay swap through sdk/swaps, starts or reuses the daemon
// background worker for the resulting payment hash, and returns the initial
// durable summary to the RPC caller.
func (s *swapClientService) StartPay(ctx context.Context,
	req *swapclientrpc.StartPayRequest) (*swapclientrpc.StartPayResponse,
	error) {

	if req.GetInvoice() == "" {
		return nil, status.Error(
			codes.InvalidArgument, "invoice is required",
		)
	}
	if req.GetIdempotencyKey() != "" {
		return nil, status.Error(
			codes.Unimplemented,
			"idempotency_key is reserved for future use",
		)
	}

	session, err := s.client.StartPayViaLightning(
		ctx, req.GetInvoice(), req.GetMaxFeeSat(),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "start pay swap: %v",
			err)
	}

	hash := session.PaymentHash()
	s.startPayWorker(hash)

	summary, err := s.summaryByHash(ctx, hash)
	if err != nil {
		return nil, err
	}

	return &swapclientrpc.StartPayResponse{
		PaymentHash: hex.EncodeToString(hash[:]),
		Swap:        summary,
	}, nil
}

// StartReceive persists a receive swap through sdk/swaps, starts or reuses the
// daemon background worker for the resulting payment hash, and returns the
// invoice plus initial durable summary to the RPC caller.
func (s *swapClientService) StartReceive(ctx context.Context,
	req *swapclientrpc.StartReceiveRequest) (
	*swapclientrpc.StartReceiveResponse, error) {

	if req.GetAmountSat() <= 0 {
		return nil, status.Error(
			codes.InvalidArgument, "amount_sat must be positive",
		)
	}
	if req.GetIdempotencyKey() != "" {
		return nil, status.Error(
			codes.Unimplemented,
			"idempotency_key is reserved for future use",
		)
	}

	session, err := s.client.StartReceiveViaLightning(
		ctx,
		btcutil.Amount(
			req.GetAmountSat(),
		),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "start receive "+
			"swap: %v", err)
	}

	hash := session.PaymentHash()
	s.startReceiveWorker(hash)

	summary, err := s.summaryByHash(ctx, hash)
	if err != nil {
		return nil, err
	}

	return &swapclientrpc.StartReceiveResponse{
		PaymentHash: hex.EncodeToString(hash[:]),
		Invoice:     session.Invoice(),
		Swap:        summary,
	}, nil
}

// ResumeSwap is a manual wake-up path for a persisted swap. It does not create
// an independent execution path: if a worker for the payment hash is already
// active, the existing worker remains the sole owner and the current summary is
// returned.
func (s *swapClientService) ResumeSwap(ctx context.Context,
	req *swapclientrpc.ResumeSwapRequest) (
	*swapclientrpc.ResumeSwapResponse, error) {

	hash, err := parsePaymentHash(req.GetPaymentHash())
	if err != nil {
		return nil, err
	}

	switch req.GetDirection() {
	case swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY:
		s.startPayWorker(hash)

	case swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE:
		s.startReceiveWorker(hash)

	default:
		return nil, status.Error(
			codes.InvalidArgument, "direction is required",
		)
	}

	summary, err := s.summaryByHash(ctx, hash)
	if err != nil {
		return nil, err
	}

	return &swapclientrpc.ResumeSwapResponse{Swap: summary}, nil
}

// ListSwaps exposes the daemon-owned swap store through the client RPC
// contract. The pending filter is delegated to sdk/swaps so the daemon service
// does not need to duplicate terminal-state knowledge.
func (s *swapClientService) ListSwaps(ctx context.Context,
	req *swapclientrpc.ListSwapsRequest) (*swapclientrpc.ListSwapsResponse,
	error) {

	summaries, err := s.client.ListSwapSummaries(
		ctx, req.GetPendingOnly(),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list swaps: %v", err)
	}

	resp := &swapclientrpc.ListSwapsResponse{
		Swaps: make([]*swapclientrpc.SwapSummary, 0, len(summaries)),
	}
	for i := range summaries {
		summary := swapSummaryToProto(summaries[i])
		resp.Swaps = append(resp.Swaps, summary)
	}

	return resp, nil
}

// GetSwap reads one persisted swap summary by payment hash. The lookup is
// performed over sdk/swaps summaries so pay and receive swaps share the same
// response path.
func (s *swapClientService) GetSwap(ctx context.Context,
	req *swapclientrpc.GetSwapRequest) (*swapclientrpc.GetSwapResponse,
	error) {

	hash, err := parsePaymentHash(req.GetPaymentHash())
	if err != nil {
		return nil, err
	}

	summary, err := s.summaryByHash(ctx, hash)
	if err != nil {
		return nil, err
	}

	return &swapclientrpc.GetSwapResponse{Swap: summary}, nil
}

// SubscribeSwaps streams daemon-observed summary changes. The first slice emits
// existing rows on request and publishes a fresh summary whenever a daemon
// worker exits; richer step-by-step events can be added without changing who
// owns swap execution.
func (s *swapClientService) SubscribeSwaps(
	req *swapclientrpc.SubscribeSwapsRequest,
	stream grpc.ServerStreamingServer[swapclientrpc.SubscribeSwapsResponse],
) error {

	if req.GetIncludeExisting() {
		existing, err := s.ListSwaps(
			stream.Context(), &swapclientrpc.ListSwapsRequest{
				PendingOnly: req.GetPendingOnly(),
			},
		)
		if err != nil {
			return err
		}

		for _, summary := range existing.GetSwaps() {
			if err := stream.Send(
				&swapclientrpc.SubscribeSwapsResponse{
					Swap: summary,
				},
			); err != nil {
				return err
			}
		}
	}

	ch := s.addSubscriber()
	defer s.removeSubscriber(ch)

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()

		case summary := <-ch:
			if req.GetPendingOnly() && !summary.GetPending() {
				continue
			}

			if err := stream.Send(
				&swapclientrpc.SubscribeSwapsResponse{
					Swap: summary,
				},
			); err != nil {
				return err
			}
		}
	}
}

// resumePending starts background workers for every persisted non-terminal
// swap reported by sdk/swaps. This is the restart-resume hook: when a
// swapruntime daemon starts, the store is scanned before the RPC server begins
// accepting control calls.
func (s *swapClientService) resumePending(ctx context.Context) {
	summaries, err := s.client.ListSwapSummaries(ctx, true)
	if err != nil {
		s.log.Warnf("unable to list pending swaps on startup: %v", err)

		return
	}

	for _, summary := range summaries {
		switch summary.Direction {
		case swaps.SwapDirectionPay:
			s.startPayWorker(summary.PaymentHash)

		case swaps.SwapDirectionReceive:
			s.startReceiveWorker(summary.PaymentHash)
		}
	}
}

// startPayWorker claims process-local ownership for a pay swap FSM and runs
// the sdk/swaps resume-and-wait path in a goroutine. Duplicate starts for the
// same payment hash return immediately, so one daemon process has at most one
// active pay driver per hash.
func (s *swapClientService) startPayWorker(hash lntypes.Hash) {
	key := hex.EncodeToString(hash[:])
	if !s.markActive(key) {
		return
	}

	go func() {
		defer s.markInactive(key)
		defer s.publishHash(hash)

		session, err := s.client.ResumePayViaLightning(s.rootCtx, hash)
		if err != nil {
			s.log.Warnf("unable to resume pay swap %s: %v", key,
				err)

			return
		}

		_, err = session.Wait(s.rootCtx)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.log.Warnf("pay swap %s stopped: %v", key, err)
		}
	}()
}

// startReceiveWorker claims process-local ownership for a receive swap FSM and
// runs the sdk/swaps resume-and-wait path in a goroutine. Duplicate starts for
// the same payment hash return immediately, preserving one active receive
// driver per hash.
func (s *swapClientService) startReceiveWorker(hash lntypes.Hash) {
	key := hex.EncodeToString(hash[:])
	if !s.markActive(key) {
		return
	}

	go func() {
		defer s.markInactive(key)
		defer s.publishHash(hash)

		session, err := s.client.ResumeReceiveViaLightning(
			s.rootCtx, hash,
		)
		if err != nil {
			s.log.Warnf("unable to resume receive swap %s: %v", key,
				err)

			return
		}

		_, err = session.Wait(s.rootCtx)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.log.Warnf("receive swap %s stopped: %v", key, err)
		}
	}()
}

// markActive records that the daemon has an in-process worker responsible for
// a payment hash. It returns false when another goroutine already owns that
// hash, which is the worker-deduplication gate used by start/resume paths.
func (s *swapClientService) markActive(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.active[key]; ok {
		return false
	}

	s.active[key] = struct{}{}

	return true
}

// markInactive releases process-local worker ownership for a payment hash once
// the worker has exited.
func (s *swapClientService) markInactive(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.active, key)
}

// addSubscriber registers one buffered summary channel for SubscribeSwaps.
// The buffer keeps worker exit publication from blocking on slow clients.
func (s *swapClientService) addSubscriber() chan *swapclientrpc.SwapSummary {
	ch := make(chan *swapclientrpc.SwapSummary, 16)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.subscribers[ch] = struct{}{}

	return ch
}

// removeSubscriber unregisters and closes a SubscribeSwaps channel when the
// streaming RPC returns.
func (s *swapClientService) removeSubscriber(
	ch chan *swapclientrpc.SwapSummary) {

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.subscribers, ch)
	close(ch)
}

// publishHash reads the latest persisted summary for a payment hash and offers
// it to every active subscriber. Slow subscribers may miss an update, but they
// can always recover current state through ListSwaps or GetSwap.
func (s *swapClientService) publishHash(hash lntypes.Hash) {
	summary, err := s.summaryByHash(s.rootCtx, hash)
	if err != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for ch := range s.subscribers {
		select {
		case ch <- summary:
		default:
		}
	}
}

// summaryByHash finds the current persisted summary for a payment hash.
func (s *swapClientService) summaryByHash(ctx context.Context,
	hash lntypes.Hash) (*swapclientrpc.SwapSummary, error) {

	summary, err := s.client.GetSwapSummary(ctx, hash)
	if err != nil {
		if errors.Is(err, swaps.ErrSwapSummaryNotFound) {
			return nil, status.Error(
				codes.NotFound, "swap not found",
			)
		}

		return nil, status.Errorf(codes.Internal, "get swap "+
			"summary: %v", err)
	}

	return swapSummaryToProto(summary), nil
}

// parsePaymentHash decodes the RPC payment_hash string and enforces the fixed
// Lightning payment-hash size before it reaches sdk/swaps.
func parsePaymentHash(encoded string) (lntypes.Hash, error) {
	raw, err := hex.DecodeString(encoded)
	if err != nil {
		return lntypes.Hash{}, status.Errorf(codes.InvalidArgument,
			"decode payment_hash: %v", err)
	}
	if len(raw) != lntypes.HashSize {
		return lntypes.Hash{}, status.Errorf(codes.InvalidArgument,
			"payment_hash must be %d bytes", lntypes.HashSize)
	}

	var hash lntypes.Hash
	copy(hash[:], raw)

	return hash, nil
}

// swapSummaryToProto translates the SDK's durable summary model into the
// daemon-owned swapclientrpc summary model.
func swapSummaryToProto(summary swaps.SwapSummary) *swapclientrpc.SwapSummary {
	return &swapclientrpc.SwapSummary{
		Direction:        swapDirectionToProto(summary.Direction),
		PaymentHash:      hex.EncodeToString(summary.PaymentHash[:]),
		State:            swapStateToProto(summary.State),
		Pending:          summary.Pending,
		AmountSat:        summary.AmountSat,
		FeeSat:           summary.FeeSat,
		MaxFeeSat:        summary.MaxFeeSat,
		VhtlcOutpoint:    summary.VHTLCOutpoint,
		VhtlcAmountSat:   summary.VHTLCAmountSat,
		FundingSessionId: summary.FundingSessionID,
		ClaimSessionId:   summary.ClaimSessionID,
		RefundSessionId:  summary.RefundSessionID,
		TerminalReason:   summary.TerminalReason,
		CreatedAtUnix:    summary.CreatedAt.Unix(),
		UpdatedAtUnix:    summary.UpdatedAt.Unix(),
		DeadlineUnix:     summary.Deadline.Unix(),
		RefundLocktime:   summary.RefundLocktime,
	}
}

// swapStateToProto maps sdk/swaps persisted state names into the stable public
// RPC enum. Unknown state names map to UNSPECIFIED so callers can detect that a
// newer daemon or SDK state is not understood by their current client.
func swapStateToProto(state string) swapclientrpc.SwapState {
	switch state {
	case "Created":
		return swapclientrpc.SwapState_SWAP_STATE_CREATED

	case "SwapCreated":
		return swapclientrpc.SwapState_SWAP_STATE_SWAP_CREATED

	case "FundingInitiated":
		return swapclientrpc.SwapState_SWAP_STATE_FUNDING_INITIATED

	case "VHTLCFunded":
		return swapclientrpc.SwapState_SWAP_STATE_VHTLC_FUNDED

	case "WaitingForClaim":
		return swapclientrpc.SwapState_SWAP_STATE_WAITING_FOR_CLAIM

	case "Completed":
		return swapclientrpc.SwapState_SWAP_STATE_COMPLETED

	case "Expired":
		return swapclientrpc.SwapState_SWAP_STATE_EXPIRED

	case "RefundInitiated":
		return swapclientrpc.SwapState_SWAP_STATE_REFUND_INITIATED

	case "Refunded":
		return swapclientrpc.SwapState_SWAP_STATE_REFUNDED

	case "NeedsIntervention":
		return swapclientrpc.SwapState_SWAP_STATE_NEEDS_INTERVENTION

	case "Failed":
		return swapclientrpc.SwapState_SWAP_STATE_FAILED

	case "InvoiceCreated":
		return swapclientrpc.SwapState_SWAP_STATE_INVOICE_CREATED

	case "ClaimInitiated":
		return swapclientrpc.SwapState_SWAP_STATE_CLAIM_INITIATED

	default:
		return swapclientrpc.SwapState_SWAP_STATE_UNSPECIFIED
	}
}

// swapDirectionToProto maps sdk/swaps directions into the public RPC direction
// enum used by daemon callers.
func swapDirectionToProto(
	direction swaps.SwapDirection) swapclientrpc.SwapDirection {

	switch direction {
	case swaps.SwapDirectionPay:
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY

	case swaps.SwapDirectionReceive:
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE

	default:
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_UNSPECIFIED
	}
}
