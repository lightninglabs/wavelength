//go:build swapruntime

package swapclientserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightninglabs/wavelength/lib/types"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/rpc/restclient"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	sdkark "github.com/lightninglabs/wavelength/sdk/ark"
	"github.com/lightninglabs/wavelength/sdk/swaps"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
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

	// daemonConn is the in-process Ark/daemon surface used by the credit
	// bridge for identity-key, receive-script, and indexed-VTXO lookups.
	// Kept so Register can build the credit CreditDaemon adapter.
	daemonConn swaps.DaemonConn

	// log is the swapruntime subsystem logger derived from waved's logger
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

	// receiveMinAmount returns the current minimum output amount that a
	// receive-swap vHTLC must satisfy before it is safe to hand a BOLT-11
	// invoice to a payer. Production derives this from the daemon's cached
	// operator terms; tests may leave it nil to disable this preflight.
	receiveMinAmount func(context.Context) (uint64, error)

	// payMinAmount returns the current minimum output amount that a
	// pay-swap vHTLC must satisfy before sdk/swaps persists the pay
	// session. This mirrors the receive guard so high-level wallet RPC
	// callers fail before a background worker reaches the daemon's SendOOR
	// dust check.
	payMinAmount func(context.Context) (uint64, error)

	// chainParams decodes BOLT-11 pay invoices for local amount preflight
	// and duplicate in-flight checks before swap creation mutates remote
	// swapdk-server state.
	chainParams *chaincfg.Params
}

// operatorPubKeyFetcher returns the current Ark operator pubkey. It exists so
// daemon-hosted swap clients can bypass the cached GetInfo snapshot when they
// construct fresh vHTLC policies.
type operatorPubKeyFetcher func(context.Context) (*btcec.PublicKey, error)

// liveOperatorDaemonConn delegates all daemon operations to the embedded
// connection except OperatorPubKey, which is fetched live from the daemon's
// direct Ark transport. This keeps ordinary status reads on the cached GetInfo
// path while ensuring newly-created swap vHTLC policies see operator-key
// rotations before OOR funding is submitted.
type liveOperatorDaemonConn struct {
	swaps.DaemonConn

	operatorPubKey operatorPubKeyFetcher
}

// OperatorPubKey returns the latest operator key from the direct fetcher.
func (d *liveOperatorDaemonConn) OperatorPubKey(ctx context.Context) (
	*btcec.PublicKey, error) {

	return d.operatorPubKey(ctx)
}

// daemonWithLiveOperatorKey wraps daemon so swap policy construction does not
// consume a stale operator key from GetInfo's cached server-info snapshot.
func daemonWithLiveOperatorKey(daemon swaps.DaemonConn,
	fetch operatorPubKeyFetcher) swaps.DaemonConn {

	if fetch == nil {
		return daemon
	}

	return &liveOperatorDaemonConn{
		DaemonConn:     daemon,
		operatorPubKey: fetch,
	}
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
	// QuotePayViaLightning previews a pay swap without creating durable
	// state or scheduling a worker.
	QuotePayViaLightning(context.Context, string,
		uint64) (*swaps.InSwapQuote, error)

	// StartPayViaLightning creates a new pay swap for a Lightning invoice
	// and persists enough state for a background worker to resume it by
	// payment hash.
	StartPayViaLightning(context.Context, string,
		uint64) (paySwapSession, error)

	// StartReceiveViaLightning creates a new receive swap and returns the
	// invoice that callers hand to the remote payer.
	StartReceiveViaLightning(context.Context, btcutil.Amount,
		string) (receiveSwapSession, error)

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

// swapServerClients holds all outbound clients backed by one configured
// swapdk-server transport.
type swapServerClients struct {
	server  swaps.SwapServerConn
	mailbox mailboxpb.MailboxServiceClient
	cleanup func() error
}

// clientTLSCertProvider returns the daemon identity certificate at handshake
// time, after the wallet has had a chance to derive the identity key.
type clientTLSCertProvider func() ([]tls.Certificate, error)

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
// The function is called only from a swapruntime-tagged waved binary. It
// opens the daemon-owned swap store, constructs the sdk/swaps client, registers
// the separate swapclientrpc subserver on the existing daemon listener, and
// resumes all persisted pending swap sessions before returning. The returned
// cleanup function stops background workers and closes the swap store and
// swapdk-server connection during daemon shutdown.
//
// When cfg.Swap.SuppressResume is true, the synchronous resumePending sweep is
// skipped so a higher layer (the wavewalletrpc subserver) can own the unified
// resume policy. In that case cfg.Swap.Backend is populated so the higher layer
// can drive ResumePending itself after performing any cross-subsystem
// preconditions.
func Register(ctx context.Context, grpcServer *grpc.Server,
	rpcServer *waved.RPCServer, cfg *waved.Config) (func(), error) {

	svc, cleanup, err := newSwapClientService(ctx, rpcServer, cfg)
	if err != nil {
		return nil, err
	}

	swapclientrpc.RegisterSwapClientServiceServer(grpcServer, svc)

	// Publish the backend handle so the wavewalletrpc registrar (if
	// compiled in) can drive ResumePending and any future in-Go calls
	// without going through the gRPC stub. Done before the resume sweep so
	// the handle is reachable even when this layer is configured to skip
	// its own sweep.
	if cfg.Swap != nil {
		cfg.Swap.Backend = svc

		// Publish the credit bridges so the daemon can construct the
		// credit durable-actor subsystem over the swap-server credit
		// surface and the wallet/daemon surface.
		cfg.Swap.CreditServer = &creditServerBridge{svc: svc}
		cfg.Swap.CreditDaemon = &creditDaemonBridge{
			daemon: svc.daemonConn,
			rpc:    rpcServer,
		}
	}

	suppressResume := cfg.Swap != nil && cfg.Swap.SuppressResume
	if !suppressResume {
		svc.resumePending(ctx)
	}

	return cleanup, nil
}

// ResumePending re-arms background workers for every persisted pending swap
// session. It is idempotent: payment hashes already owned by an active worker
// are skipped. The method satisfies waved.SwapBackend so a higher subserver
// (such as wavewalletrpc) can drive the resume sweep as part of a unified
// wallet-level lifecycle policy.
func (s *swapClientService) ResumePending(ctx context.Context) {
	s.resumePending(ctx)
}

// RegisterGateway installs the optional SwapClientService handlers on the
// daemon HTTP/JSON gateway.
func RegisterGateway(ctx context.Context, mux *runtime.ServeMux,
	endpoint string, opts []grpc.DialOption, _ *waved.RPCServer,
	_ *waved.Config) error {

	return swapclientrpc.RegisterSwapClientServiceHandlerFromEndpoint(
		ctx, mux, endpoint, opts,
	)
}

// newSwapClientService builds the daemon-owned swap executor from waved
// runtime dependencies.
//
// The constructor opens the daemon swap store, dials swapdk-server, creates an
// in-process Ark SDK facade over waved's existing DaemonService, and wires
// the sdk/swaps client. Receive-auth signing and ECDH are delegated back to the
// daemon through the Ark SDK facade, so the swapruntime layer does not persist
// its own receive-auth key material. It also returns a cleanup function that
// must be called during daemon shutdown so the root worker context is canceled
// before the Ark, swapdk-server, and store resources are closed.
//
//nolint:contextcheck
func newSwapClientService(ctx context.Context, rpcServer *waved.RPCServer,
	daemonCfg *waved.Config) (*swapClientService, func(), error) {

	if daemonCfg == nil {
		return nil, nil, fmt.Errorf("daemon config is required")
	}

	cfg := daemonCfg.Swap
	if cfg == nil {
		cfg = waved.DefaultConfig().Swap
	}

	dbPath, err := swapStoreDatabasePath(daemonCfg, cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := ensureSwapDBDir(dbPath); err != nil {
		return nil, nil, fmt.Errorf("create swap db dir: %w", err)
	}

	log := btclog.Disabled
	if rpcServer != nil {
		log = rpcServer.SubLogger(waved.SwapSubsystem)
	}

	store, err := swaps.NewSqliteStore(&swaps.SqliteStoreConfig{
		DatabaseFileName: dbPath,
	}, log)
	if err != nil {
		return nil, nil, fmt.Errorf("open swap store: %w", err)
	}
	if err := rpcServer.RegisterVHTLCRecoveryPreimageResolver(
		store,
	); err != nil {

		_ = store.Close()

		return nil, nil, fmt.Errorf("register recovery preimage "+
			"resolver: %w", err)
	}

	// Resolve through a shallow config copy so the nil-Swap fallback above
	// participates in the same network+transport lookup without mutating
	// the caller-owned daemon config.
	resolvedCfg := *daemonCfg
	resolvedCfg.Swap = cfg
	swapAddr := resolvedCfg.SwapServerAddress()

	var clientCerts clientTLSCertProvider
	if !cfg.ServerInsecure {
		clientCerts = rpcServer.ClientTLSCerts
	}

	swapClients, err := newSwapServerClients(
		cfg, swapAddr, rpcServer.SignMailboxAuth, clientCerts,
	)
	if err != nil {
		_ = store.Close()

		return nil, nil, err
	}

	arkClient, err := sdkark.WrapDaemonServer(ctx, sdkark.InProcessConfig{
		DaemonServer: rpcServer,
	})
	if err != nil {
		_ = swapClients.server.Close()
		_ = swapClients.cleanup()
		_ = store.Close()

		return nil, nil, fmt.Errorf("create in-process ark client: %w",
			err)
	}

	chainParams, err := chainParamsForNetwork(daemonCfg.Network)
	if err != nil {
		_ = arkClient.Close()
		_ = swapClients.server.Close()
		_ = swapClients.cleanup()
		_ = store.Close()

		return nil, nil, err
	}

	invoiceGen, err := daemonInvoiceGenerator(arkClient, chainParams)
	if err != nil {
		_ = arkClient.Close()
		_ = swapClients.server.Close()
		_ = swapClients.cleanup()
		_ = store.Close()

		return nil, nil, err
	}

	rootCtx, cancel := context.WithCancel(ctx)
	daemonConn := daemonWithLiveOperatorKey(
		arkClient, rpcServer.OperatorPubKey,
	)
	swapClient := swaps.NewSwapClientWithStore(
		swapClients.server, daemonConn, log, invoiceGen, store,
	)
	swapClient.SetChainParams(chainParams)
	recoveryCfg := cfg.VHTLCRecovery
	swapClient.SetRecoveryPolicy(swaps.RecoveryPolicy{
		AutoEscalate: recoveryCfg.AutoEscalate,
		CooperativeFailureGracePeriod: recoveryCfg.
			CooperativeFailureGracePeriod,
		MinRecoveryMarginBlocks: recoveryCfg.MinRecoveryMarginBlocks,
		MaxFeeRateSatPerKW:      recoveryCfg.MaxFeeRateSatPerKW,
	})

	// The out-swap event receiver must be wired before any
	// ReceiveViaLightning starts: SwapClient captures the receiver into the
	// receive worker at start time, so a late SetOutSwapEventReceiver would
	// leave already-running workers using whatever receiver (if any) was
	// installed earlier. resumePending below kicks off persisted workers,
	// so installing the receiver here, immediately after construction, is
	// the only correct point. An empty mailbox ID makes the receiver derive
	// the per-swap mailbox from the client identity key and payment hash.
	//
	// We pass the service logger so the receiver's pull retry attempts
	// surface in operator logs alongside the equivalent serverconn ingress
	// WARNs; without this the SDK side flaps silently while the daemon
	// ingress logs the same endpoint failures, which is the visibility gap
	// that made #505 hard to diagnose from logs alone.
	swapClient.SetOutSwapEventReceiver(
		swaps.NewMailboxOutSwapEventReceiver(
			swapClients.mailbox, "",
			swaps.WithMailboxReceiverLog(log),
		),
	)
	if err := installVTXOForfeitParticipantSigner(
		rpcServer, swapClients.server,
	); err != nil {

		_ = arkClient.Close()
		_ = swapClients.server.Close()
		_ = swapClients.cleanup()
		_ = store.Close()

		return nil, nil, err
	}

	minAmount := func(ctx context.Context) (uint64, error) {
		info, err := arkClient.GetInfo(ctx)
		if err != nil {
			return 0, err
		}

		return vtxoMinAmountSat(info.ServerInfo)
	}

	service := &swapClientService{
		client: &swapClientAdapter{
			client: swapClient,
		},
		store:      store,
		daemonConn: daemonConn,
		log:        log,
		rootCtx:    rootCtx,
		cancel:     cancel,
		active:     make(map[string]struct{}),
		subscribers: make(
			map[chan *swapclientrpc.SwapSummary]struct{},
		),
		receiveMinAmount: minAmount,
		payMinAmount:     minAmount,
		chainParams:      chainParams,
	}

	cleanup := func() {
		cancel()
		_ = arkClient.Close()
		_ = swapClients.server.Close()
		_ = swapClients.cleanup()
		_ = store.Close()
	}

	return service, cleanup, nil
}

// swapStoreDatabasePath returns the daemon-owned swap store path. By default it
// follows the network-scoped daemon DB directory so a network DB reset also
// clears wallet activity derived from persisted swap sessions.
func swapStoreDatabasePath(daemonCfg *waved.Config,
	cfg *waved.SwapConfig) (string, error) {

	if daemonCfg == nil {
		return "", fmt.Errorf("daemon config is required")
	}
	if cfg != nil && cfg.DatabaseFileName != "" {
		return cfg.DatabaseFileName, nil
	}

	return filepath.Join(
		daemonCfg.NetworkDir(), swaps.DefaultSqliteDatabaseFileName,
	), nil
}

func installVTXOForfeitParticipantSigner(rpcServer *waved.RPCServer,
	swapServer swaps.SwapServerConn) error {

	if rpcServer == nil {
		return fmt.Errorf("daemon RPC server is required")
	}
	if swapServer == nil {
		return fmt.Errorf("swap server connection is required")
	}

	return rpcServer.SetVTXOForfeitParticipantSigner(
		func(ctx context.Context,
			req *vtxo.ForfeitParticipantSignRequest) (
			[]*types.ForfeitParticipantSig, error) {

			payload, err := swaps.ForfeitSignaturePayloadFromVTXORequest(
				req,
			)
			if err != nil {
				return nil, err
			}

			sig, err := swapServer.SignInSwapForfeit(ctx, payload)
			if err != nil {
				return nil, err
			}

			pubKey, err := btcec.ParsePubKey(sig.PubKey)
			if err != nil {
				return nil, fmt.Errorf("parse in-swap forfeit "+
					"participant pubkey: %w", err)
			}
			signature, err := schnorr.ParseSignature(sig.Signature)
			if err != nil {
				return nil, fmt.Errorf("parse in-swap forfeit "+
					"participant signature: %w", err)
			}

			return []*types.ForfeitParticipantSig{{
				PubKey:    pubKey,
				Signature: signature,
			}}, nil
		},
	)
}

// newSwapServerClients builds the swapdk-server clients for the configured
// daemon-owned outbound transport.
func newSwapServerClients(cfg *waved.SwapConfig, swapAddr string,
	sign serverconn.MailboxAuthSigner,
	clientCerts clientTLSCertProvider) (*swapServerClients, error) {

	switch cfg.ServerTransport {
	case "", waved.RPCTransportGRPC:
		dialOpts, err := swapServerDialOptions(
			cfg, swapAddr, clientCerts,
		)
		if err != nil {
			return nil, err
		}

		swapConn, err := grpc.NewClient(swapAddr, dialOpts...)
		if err != nil {
			return nil, fmt.Errorf("connect to swap server: %w",
				err)
		}

		return &swapServerClients{
			server: swaps.NewGRPCSwapServerConn(swapConn),
			mailbox: serverconn.NewAuthenticatedMailboxClient(
				mailboxpb.NewMailboxServiceClient(swapConn),
				sign,
			),
			cleanup: swapConn.Close,
		}, nil

	case waved.RPCTransportREST:
		opts, err := swapServerRESTOptions(cfg, clientCerts)
		if err != nil {
			return nil, err
		}

		baseURL := swapServerRESTBaseURL(cfg, swapAddr)
		transport := restclient.New(baseURL, opts...)

		return &swapServerClients{
			server: swaps.NewRESTSwapServerConn(baseURL, opts...),
			mailbox: serverconn.NewAuthenticatedMailboxClient(
				restclient.NewMailboxServiceClientFromClient(
					transport,
				),
				sign,
			),
			cleanup: func() error { return nil },
		}, nil

	default:
		return nil, fmt.Errorf("unknown swap server transport %q",
			cfg.ServerTransport)
	}
}

// StartPayViaLightning starts a real sdk/swaps pay session and returns it
// through the narrow paySwapSession interface expected by the daemon service.
func (a *swapClientAdapter) StartPayViaLightning(ctx context.Context,
	invoice string, maxFeeSat uint64) (paySwapSession, error) {

	return a.client.StartPayViaLightning(ctx, invoice, maxFeeSat)
}

// StartPayViaLightningWithCredits starts a real sdk/swaps pay session with
// optional credit use.
func (a *swapClientAdapter) StartPayViaLightningWithCredits(ctx context.Context,
	invoice string, maxFeeSat uint64, maxCreditSat uint64) (paySwapSession,
	error) {

	return a.client.StartPayViaLightningWithCredits(
		ctx, invoice, maxFeeSat, maxCreditSat,
	)
}

// QuotePayViaLightning previews a pay swap without creating durable state.
func (a *swapClientAdapter) QuotePayViaLightning(ctx context.Context,
	invoice string, maxFeeSat uint64) (*swaps.InSwapQuote, error) {

	return a.client.QuotePayViaLightning(ctx, invoice, maxFeeSat)
}

// QuotePayViaLightningWithCredits previews a pay swap with optional credit use.
func (a *swapClientAdapter) QuotePayViaLightningWithCredits(ctx context.Context,
	invoice string, maxFeeSat uint64, maxCreditSat uint64) (
	*swaps.InSwapQuote, error) {

	return a.client.QuotePayViaLightningWithCredits(
		ctx, invoice, maxFeeSat, maxCreditSat,
	)
}

// CreateCredit forwards credit funding requests to sdk/swaps.
func (a *swapClientAdapter) CreateCredit(ctx context.Context,
	req swaps.CreateCreditRequest) (*swaps.CreditOperation, error) {

	return a.client.CreateCredit(ctx, req)
}

// RedeemCredit forwards credit redemption requests to sdk/swaps.
func (a *swapClientAdapter) RedeemCredit(ctx context.Context,
	req swaps.RedeemCreditRequest) (*swaps.CreditRedemption, error) {

	return a.client.RedeemCredit(ctx, req)
}

// ListCredits forwards credit account snapshots to sdk/swaps.
func (a *swapClientAdapter) ListCredits(ctx context.Context, limit uint32) (
	*swaps.CreditSnapshot, error) {

	return a.client.ListCredits(ctx, limit)
}

// StartReceiveViaLightning starts a real sdk/swaps receive session and wraps
// it with method accessors for the daemon RPC response path.
func (a *swapClientAdapter) StartReceiveViaLightning(ctx context.Context,
	amountSat btcutil.Amount, memo string) (receiveSwapSession, error) {

	session, err := a.client.StartReceiveViaLightning(ctx, amountSat, memo)
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
func (d *daemonAuthOnlyInvoiceCreator) CreateInvoiceWithKey(ctx context.Context,
	amountSat btcutil.Amount, memo string, routeHint *swaps.RouteHint,
	expiry time.Duration, authKey keychain.SingleKeyMessageSigner,
	preimage *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash, error) {

	return d.inner.CreateInvoiceWithKey(
		ctx, amountSat, memo, routeHint, expiry, authKey, preimage,
	)
}

// CreateInvoiceWithKeyRouteHintPaths delegates to the underlying generator
// with every private route-hint path and the daemon-derived auth key.
func (d *daemonAuthOnlyInvoiceCreator) CreateInvoiceWithKeyRouteHintPaths(
	ctx context.Context, amountSat btcutil.Amount, memo string,
	routeHintPaths [][]*swaps.RouteHint, expiry time.Duration,
	authKey keychain.SingleKeyMessageSigner, preimage *lntypes.Preimage) (
	*invoices.Invoice, lntypes.Hash, error) {

	return d.inner.CreateInvoiceWithKeyRouteHintPaths(
		ctx, amountSat, memo, routeHintPaths, expiry, authKey, preimage,
	)
}

// swapServerDialOptions maps daemon swap config into gRPC transport options
// for swapdk-server.
func swapServerDialOptions(cfg *waved.SwapConfig, addr string,
	clientCerts clientTLSCertProvider) ([]grpc.DialOption, error) {

	// Swap operations wait for the configured server to reconnect, while
	// best-effort reads fail fast so they cannot consume an enclosing
	// wallet RPC's entire deadline.
	waitForReadyInterceptorOpt := grpc.WithChainUnaryInterceptor(
		swapServerWaitForReadyInterceptor,
	)

	switch {
	case cfg.ServerTLSCertPath != "":
		tlsCfg, err := swapServerTLSConfig(
			cfg.ServerTLSCertPath, clientCerts,
		)
		if err != nil {
			return nil, err
		}

		return []grpc.DialOption{
			grpc.WithTransportCredentials(
				credentials.NewTLS(tlsCfg),
			),
			waitForReadyInterceptorOpt,
		}, nil

	case useInsecureSwapServerTransport(cfg, addr):
		return []grpc.DialOption{
			grpc.WithTransportCredentials(
				insecure.NewCredentials(),
			),
			waitForReadyInterceptorOpt,
		}, nil

	default:
		tlsCfg := &tls.Config{
			GetClientCertificate: swapClientCertificate(
				clientCerts,
			),
			MinVersion: tls.VersionTLS12,
		}

		return []grpc.DialOption{
			grpc.WithTransportCredentials(
				credentials.NewTLS(tlsCfg),
			),
			waitForReadyInterceptorOpt,
		}, nil
	}
}

// swapServerWaitForReadyInterceptor applies reconnect waiting only to swap
// operations that create or advance protocol state. User operations remain
// bounded by their request context, while background resume operations remain
// bounded by the daemon root context. Read-only quotes and credit snapshots
// retain gRPC's fail-fast behavior.
func swapServerWaitForReadyInterceptor(ctx context.Context, method string, req,
	reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption) error {

	if swapServerOperationWaitsForReady(method) {
		opts = append(opts, grpc.WaitForReady(true))
	}

	return invoker(ctx, method, req, reply, cc, opts...)
}

// swapServerOperationWaitsForReady reports whether an unavailable swap server
// should delay the RPC until the caller's context expires or the server
// reconnects.
func swapServerOperationWaitsForReady(method string) bool {
	switch method {
	case swaprpc.SwapService_RequestChannelId_FullMethodName,
		swaprpc.SwapService_CreateInSwap_FullMethodName,
		swaprpc.SwapService_CreateCredit_FullMethodName,
		swaprpc.SwapService_RedeemCredit_FullMethodName,
		swaprpc.SwapService_AuthorizeInSwapRefund_FullMethodName,
		swaprpc.SwapService_AcknowledgeOutSwapHtlc_FullMethodName,
		swaprpc.SwapService_SignInSwapForfeit_FullMethodName,
		swaprpc.SwapService_SubmitOutSwapForfeitSignature_FullMethodName:
		return true

	default:
		// Future RPCs fail fast until their retry and recovery
		// semantics are explicitly classified above.
		return false
	}
}

// swapServerRESTOptions maps the swapdk-server TLS config into the shared REST
// transport.
func swapServerRESTOptions(cfg *waved.SwapConfig,
	clientCerts clientTLSCertProvider) ([]restclient.Option, error) {

	tlsCfg := &tls.Config{
		GetClientCertificate: swapClientCertificate(clientCerts),
		MinVersion:           tls.VersionTLS12,
	}
	if cfg.ServerTLSCertPath != "" {
		var err error
		tlsCfg, err = swapServerTLSConfig(
			cfg.ServerTLSCertPath, clientCerts,
		)
		if err != nil {
			return nil, err
		}
	}

	httpTransport := cloneDefaultHTTPTransport()
	httpTransport.TLSClientConfig = tlsCfg

	return []restclient.Option{
		restclient.WithHTTPClient(&http.Client{
			Transport: httpTransport,
		}),
	}, nil
}

// swapServerTLSConfig builds a client TLS config pinned to the configured
// swapd certificate and carrying the daemon identity client certificate.
func swapServerTLSConfig(certPath string,
	clientCerts clientTLSCertProvider) (*tls.Config, error) {

	certBytes, err := os.ReadFile(certPath) // #nosec G304
	if err != nil {
		return nil, fmt.Errorf("load swap server TLS certificate: %w",
			err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certBytes) {
		return nil, fmt.Errorf("unable to parse swap server TLS "+
			"certificate at %s", certPath)
	}

	return &tls.Config{
		RootCAs:              pool,
		GetClientCertificate: swapClientCertificate(clientCerts),
		MinVersion:           tls.VersionTLS12,
	}, nil
}

// swapClientCertificate adapts the daemon identity certificate provider into a
// TLS handshake callback. The callback is intentionally lazy because the swap
// subserver can be registered before the daemon wallet derives its identity
// key; gRPC/HTTP retries will ask again once the wallet is ready.
func swapClientCertificate(
	clientCerts clientTLSCertProvider,
) func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {

	if clientCerts == nil {
		return nil
	}

	return func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		certs, err := clientCerts()
		if err != nil {
			return nil, err
		}
		if len(certs) == 0 {
			return &tls.Certificate{}, nil
		}

		return &certs[0], nil
	}
}

// cloneDefaultHTTPTransport returns a mutable copy of the default HTTP
// transport without relying on a forced package-global type assertion.
func cloneDefaultHTTPTransport() *http.Transport {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		}
	}

	return transport.Clone()
}

// swapServerRESTBaseURL returns the base URL used for swapdk-server
// grpc-gateway calls.
func swapServerRESTBaseURL(cfg *waved.SwapConfig, addr string) string {
	if strings.HasPrefix(addr, "http://") ||
		strings.HasPrefix(addr, "https://") {
		return addr
	}

	if cfg.ServerTLSCertPath != "" {
		return "https://" + addr
	}
	if useInsecureSwapServerTransport(cfg, addr) {
		return "http://" + addr
	}

	return "https://" + addr
}

// useInsecureSwapServerTransport reports whether the swapserver connection
// should use plaintext transport. Explicit TLS certificate pinning always wins;
// otherwise local loopback endpoints keep the historical regtest default.
func useInsecureSwapServerTransport(cfg *waved.SwapConfig, addr string) bool {
	return cfg.ServerTLSCertPath == "" &&
		(cfg.ServerInsecure || isLocalSwapServerAddr(addr))
}

// isLocalSwapServerAddr reports whether a configured swapdk-server address is
// scoped to the local machine. Local endpoints are treated as development
// endpoints and may be dialed with plaintext credentials by default.
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

// chainParamsForNetwork converts the daemon's configured network string into
// the btcd chain parameters required by the invoice generator.
func chainParamsForNetwork(network string) (*chaincfg.Params, error) {
	switch network {
	case "mainnet", "bitcoin":
		return &chaincfg.MainNetParams, nil

	case "testnet", "testnet3":
		return &chaincfg.TestNet3Params, nil

	case "testnet4":
		return &chaincfg.TestNet4Params, nil

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

// vtxoMinAmountSat returns the effective minimum VTXO output amount. Pay
// and receive swaps both create Ark VTXOs, so both rails use the same VTXO
// floor before asking the swap server for route hints or persisting local
// swap state.
func vtxoMinAmountSat(info *sdkark.ServerInfo) (uint64, error) {
	if info == nil {
		return 0, fmt.Errorf("operator terms unavailable")
	}

	if info.MinVTXOAmountSat > info.DustLimit {
		return info.MinVTXOAmountSat, nil
	}

	return info.DustLimit, nil
}

// validateReceiveAmount rejects locally-impossible receive amounts before the
// SDK requests a route hint and returns a payer-visible BOLT-11 invoice.
func (s *swapClientService) validateReceiveAmount(ctx context.Context,
	amountSat int64) error {

	if s.receiveMinAmount == nil {
		return nil
	}

	minAmountSat, err := s.receiveMinAmount(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "receive amount "+
			"preflight failed: %v", err)
	}
	if minAmountSat == 0 {
		return nil
	}
	if uint64(amountSat) >= minAmountSat {
		return nil
	}

	return status.Errorf(codes.InvalidArgument, "amount_sat %d is below "+
		"the %d sat minimum for receive swaps (operator VTXO minimum)",
		amountSat, minAmountSat)
}

// Default in-swap fee cap parameters. When the caller does not pin a max fee
// (max_fee_sat == 0), the daemon derives a proportional cap from the invoice
// amount so a normal payment routes without the caller having to reason about
// the swap server's quoted fee. The proportional rate is applied in parts per
// million for integer precision, and a small absolute floor keeps tiny swaps
// routable when 1% rounds down below the server's flat fee component.
const (
	// defaultInSwapMaxFeePPM is the proportional in-swap fee cap applied
	// when the caller does not set max_fee_sat. 10_000 ppm == 1% of the
	// invoice amount.
	defaultInSwapMaxFeePPM = 10_000

	// defaultInSwapMaxFeeFloorSat is the absolute lower bound on the
	// derived in-swap fee cap. Tiny invoices (a 5_000 sat swap quotes 1
	// sat) would otherwise floor the 1% cap below the server's flat fee, so
	// the floor guarantees a small payment still clears.
	defaultInSwapMaxFeeFloorSat = 10
)

// defaultInSwapMaxFeeSat derives the effective in-swap fee cap for a swap of
// amountSat satoshis. It returns the larger of the proportional 1% cap and the
// absolute floor so the cap never collapses below the server's flat fee on a
// small payment.
func defaultInSwapMaxFeeSat(amountSat uint64) uint64 {
	// We multiply before dividing so the proportional cap scales with the
	// invoice for amounts below 1_000_000 sat. Integer division first would
	// truncate amountSat/1_000_000 to 0 and collapse every routine swap to
	// the floor. amountSat is a uint64 sat value, so this product cannot
	// overflow at any realistic invoice size.
	proportional := amountSat * defaultInSwapMaxFeePPM / 1_000_000

	if proportional < defaultInSwapMaxFeeFloorSat {
		return defaultInSwapMaxFeeFloorSat
	}

	return proportional
}

// effectiveMaxFeeSat resolves the in-swap fee cap to forward to sdk/swaps. A
// caller-supplied non-zero max fee is honoured verbatim; a zero max fee (the
// CLI default) is replaced with the proportional ~1% default so a routine
// payment is not rejected by a 0 sat hard cap. The decoded invoice amount is
// used to size the proportional cap; if the invoice cannot be decoded the
// caller's zero is preserved so the existing downstream validation surfaces the
// original error.
func (s *swapClientService) effectiveMaxFeeSat(requested uint64,
	invoice string) uint64 {

	if requested != 0 {
		return requested
	}
	if s.chainParams == nil {
		return requested
	}

	amountSat, err := payInvoiceAmountSat(invoice, s.chainParams)
	if err != nil {
		return requested
	}

	return defaultInSwapMaxFeeSat(amountSat)
}

// errMsgInSwapFeeExceedsCap recognizes the in-swap fee-cap rejection. The
// substring is shared by both the client's own pre-funding check in
// sdk/swaps (validateInSwapQuote) and the swap server's CreateInSwap
// INVALID_ARGUMENT response, so matching on the substring catches the
// rejection regardless of which side quoted the offending fee.
const errMsgInSwapFeeExceedsCap = "exceeds max fee"

// wrapInSwapFeeError rewrites the terse "in-swap fee N exceeds max fee M"
// rejection into an actionable message that names the client-side max-fee cap
// and tells the caller how to raise it. The effective cap is included because
// the daemon may have derived it from the ~1% default rather than from a
// caller-supplied --max_fee, so a bare "max fee 0" would otherwise be
// misleading. Errors that are not a fee-cap rejection pass through untouched.
func wrapInSwapFeeError(err error, maxFeeSat uint64) error {
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), errMsgInSwapFeeExceedsCap) {
		return err
	}

	return fmt.Errorf("the swap server's quoted in-swap fee exceeds the "+
		"client's max-fee cap of %d sat; raise it with `da send "+
		"--max_fee <sat>` (the cap defaults to ~1%% of the amount "+
		"when unset): %w", maxFeeSat, err)
}

// payInvoiceAmountSat decodes the BOLT-11 invoice amount in whole satoshis.
// The swap pay path funds one Ark vHTLC of this value, so a nil, zero, or
// millisatoshi-only invoice cannot be admitted through the daemon subserver.
func payInvoiceAmountSat(invoice string,
	chainParams *chaincfg.Params) (uint64, error) {

	decoded, err := zpay32.Decode(invoice, chainParams)
	if err != nil {
		return 0, err
	}

	if decoded.MilliSat == nil {
		return 0, fmt.Errorf("invoice amount is required")
	}

	amountMSat := uint64(*decoded.MilliSat)
	if amountMSat == 0 {
		return 0, fmt.Errorf("invoice amount must be positive")
	}
	if amountMSat%1000 != 0 {
		return 0, fmt.Errorf("invoice amount must be whole satoshis")
	}

	return amountMSat / 1000, nil
}

// validatePayInvoiceAmount rejects locally-impossible pay invoices before the
// SDK persists a swap session and starts a background worker.
func (s *swapClientService) validatePayInvoiceAmount(ctx context.Context,
	invoice string) error {

	if s.payMinAmount == nil {
		return nil
	}

	if s.chainParams == nil {
		return status.Error(
			codes.Internal,
			"chain params required for pay invoice validation",
		)
	}

	amountSat, err := payInvoiceAmountSat(invoice, s.chainParams)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invoice amount "+
			"preflight failed: %v", err)
	}

	minAmountSat, err := s.payMinAmount(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "pay amount preflight "+
			"failed: %v", err)
	}
	if minAmountSat == 0 {
		return nil
	}
	if amountSat >= minAmountSat {
		return nil
	}

	return status.Errorf(codes.InvalidArgument, "invoice amount_sat %d is "+
		"below the %d sat minimum for pay swaps (operator VTXO "+
		"minimum)", amountSat, minAmountSat)
}

// quotePay previews a pay swap, routing through the credit-aware quote path
// when the SDK client supports credits so a sub-dust or credit-assisted pay is
// quoted against the caller's credit balance.
func (s *swapClientService) quotePay(ctx context.Context, invoice string,
	maxFeeSat uint64, maxCreditSat uint64) (*swaps.InSwapQuote, error) {

	if client, ok := s.client.(interface {
		QuotePayViaLightningWithCredits(context.Context, string, uint64,
			uint64) (*swaps.InSwapQuote, error)
	}); ok {
		return client.QuotePayViaLightningWithCredits(
			ctx, invoice, maxFeeSat, maxCreditSat,
		)
	}

	return s.client.QuotePayViaLightning(ctx, invoice, maxFeeSat)
}

func (s *swapClientService) startPay(ctx context.Context, invoice string,
	maxFeeSat uint64, maxCreditSat uint64) (paySwapSession, error) {

	if client, ok := s.client.(interface {
		StartPayViaLightningWithCredits(context.Context, string, uint64,
			uint64) (paySwapSession, error)
	}); ok {
		return client.StartPayViaLightningWithCredits(
			ctx, invoice, maxFeeSat, maxCreditSat,
		)
	}

	return s.client.StartPayViaLightning(ctx, invoice, maxFeeSat)
}

// QuotePay previews a pay swap through sdk/swaps without starting a daemon
// worker or creating durable swap state.
func (s *swapClientService) QuotePay(ctx context.Context,
	req *swapclientrpc.QuotePayRequest) (*swapclientrpc.QuotePayResponse,
	error) {

	if req.GetInvoice() == "" {
		return nil, status.Error(
			codes.InvalidArgument, "invoice is required",
		)
	}

	// The VTXO-minimum preflight only applies to plain Lightning pay swaps,
	// which fund a vHTLC that must clear the operator's VTXO floor. A
	// credit-eligible pay (max_credit_sat > 0) routes sub-dust amounts
	// through the credit subsystem instead of funding a sub-dust VTXO, so
	// the floor must not reject it; the server credit quote decides whether
	// credits cover the amount.
	if req.GetMaxCreditSat() == 0 {
		if err := s.validatePayInvoiceAmount(
			ctx, req.GetInvoice(),
		); err != nil {
			return nil, err
		}
	}

	maxFeeSat := s.effectiveMaxFeeSat(
		req.GetMaxFeeSat(), req.GetInvoice(),
	)

	quote, err := s.quotePay(
		ctx, req.GetInvoice(), maxFeeSat, req.GetMaxCreditSat(),
	)
	if err != nil {
		return nil, fmt.Errorf("quote pay swap: %w", wrapInSwapFeeError(
			err, maxFeeSat,
		))
	}

	return quotePayToProto(quote)
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
	existing, ok, err := s.pendingSwapForInvoice(ctx, req.GetInvoice())
	if err != nil {
		return nil, err
	}
	if ok {
		return existing, nil
	}

	maxFeeSat := s.effectiveMaxFeeSat(
		req.GetMaxFeeSat(), req.GetInvoice(),
	)

	session, err := s.startPay(
		ctx, req.GetInvoice(), maxFeeSat, req.GetMaxCreditSat(),
	)
	if err != nil {
		return nil, fmt.Errorf("start pay swap: %w", wrapInSwapFeeError(
			err, maxFeeSat,
		))
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
		req.GetMemo(),
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
		PaymentHash:        hex.EncodeToString(hash[:]),
		Invoice:            session.Invoice(),
		Swap:               summary,
		RequestedAmountSat: summary.GetRequestedAmountSat(),
		AvailableCreditSat: summary.GetAvailableCreditSat(),
		AttachedCreditSat:  summary.GetAttachedCreditSat(),
		VhtlcAmountSat:     receivePlanVHTLCAmount(summary),
		DustLimitSat:       summary.GetDustLimitSat(),
		SettlementType:     summary.GetSettlementType(),
	}, nil
}

func receivePlanVHTLCAmount(summary *swapclientrpc.SwapSummary) uint64 {
	if summary == nil {
		return 0
	}
	if summary.GetVhtlcAmountSat() > 0 {
		return uint64(summary.GetVhtlcAmountSat())
	}

	requested := summary.GetRequestedAmountSat()
	if requested == 0 {
		requested = uint64(summary.GetAmountSat())
	}

	return requested + summary.GetAttachedCreditSat()
}

// CreateCredit starts one server-owned credit funding operation for the daemon
// wallet identity account.
func (s *swapClientService) CreateCredit(ctx context.Context,
	req *swapclientrpc.CreateCreditRequest) (
	*swapclientrpc.CreateCreditResponse, error) {

	if req.GetIdempotencyKey() == "" {
		return nil, status.Error(
			codes.InvalidArgument, "idempotency_key is required",
		)
	}

	source, err := creditFundingSourceFromProto(req.GetSource())
	if err != nil {
		return nil, err
	}

	client, ok := s.client.(interface {
		CreateCredit(context.Context,
			swaps.CreateCreditRequest) (
			*swaps.CreditOperation,
			error,
		)
	})
	if !ok {
		return nil, status.Error(
			codes.Unimplemented, "credits are not supported",
		)
	}

	op, err := client.CreateCredit(ctx, swaps.CreateCreditRequest{
		IdempotencyKey: req.GetIdempotencyKey(),
		Source:         source,
		AmountSat:      req.GetAmountSat(),
		Memo:           req.GetMemo(),
	})
	if err != nil {
		return nil, fmt.Errorf("create credit: %w", err)
	}

	return creditCreateResponseToProto(op), nil
}

// RedeemCredit materializes available credits back into an Ark output.
func (s *swapClientService) RedeemCredit(ctx context.Context,
	req *swapclientrpc.RedeemCreditRequest) (
	*swapclientrpc.RedeemCreditResponse, error) {

	if req.GetIdempotencyKey() == "" {
		return nil, status.Error(
			codes.InvalidArgument, "idempotency_key is required",
		)
	}

	client, ok := s.client.(interface {
		RedeemCredit(context.Context,
			swaps.RedeemCreditRequest) (
			*swaps.CreditRedemption,
			error,
		)
	})
	if !ok {
		return nil, status.Error(
			codes.Unimplemented, "credits are not supported",
		)
	}

	result, err := client.RedeemCredit(ctx, swaps.RedeemCreditRequest{
		IdempotencyKey: req.GetIdempotencyKey(),
		AmountSat:      req.GetAmountSat(),
		DestinationPubKey: append(
			[]byte(nil), req.GetDestinationPubkey()...,
		),
	})
	if err != nil {
		return nil, fmt.Errorf("redeem credit: %w", err)
	}

	return &swapclientrpc.RedeemCreditResponse{
		OperationId: result.Operation.OperationID,
		State:       creditStateToProto(result.Operation.State),
		DebitedSat:  result.DebitedSat,
		RedeemedSat: result.RedeemedSat,
		SessionId:   result.SessionID,
	}, nil
}

// ListCredits returns the server-authoritative credit snapshot for the daemon
// wallet identity account.
func (s *swapClientService) ListCredits(ctx context.Context,
	req *swapclientrpc.ListCreditsRequest) (
	*swapclientrpc.ListCreditsResponse, error) {

	client, ok := s.client.(interface {
		ListCredits(context.Context,
			uint32) (*swaps.CreditSnapshot, error)
	})
	if !ok {
		return nil, status.Error(
			codes.Unimplemented, "credits are not supported",
		)
	}

	snapshot, err := client.ListCredits(ctx, req.GetLimit())
	if err != nil {
		return nil, fmt.Errorf("list credits: %w", err)
	}

	return creditSnapshotToProto(snapshot), nil
}

// pendingSwapForInvoice decodes a real BOLT-11 invoice and checks whether the
// daemon already has a pending swap for its payment hash. Placeholder invoices
// used by tests, or malformed invoices that sdk/swaps must reject later, skip
// this best-effort preflight so the existing validation path is preserved.
func (s *swapClientService) pendingSwapForInvoice(ctx context.Context,
	invoice string) (*swapclientrpc.StartPayResponse, bool, error) {

	if s.chainParams == nil {
		return nil, false, nil
	}

	decoded, err := zpay32.Decode(
		strings.TrimSpace(invoice), s.chainParams,
	)
	if err != nil || decoded.PaymentHash == nil {
		return nil, false, nil
	}

	var hash lntypes.Hash
	copy(hash[:], decoded.PaymentHash[:])

	summary, err := s.client.GetSwapSummary(ctx, hash)
	if err != nil {
		if errors.Is(err, swaps.ErrSwapSummaryNotFound) {
			return nil, false, nil
		}

		return nil, false, status.Errorf(codes.Internal, "get "+
			"existing swap: %v", err)
	}
	if !summary.Pending {
		return nil, false, nil
	}

	if summary.Direction != swaps.SwapDirectionPay {
		return nil, false, nil
	}

	s.startPayWorker(hash)

	return &swapclientrpc.StartPayResponse{
		PaymentHash: hex.EncodeToString(hash[:]),
		Swap:        swapSummaryToProto(summary),
	}, true, nil
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
	var senderPubKey string
	if summary.SenderPubkey != nil {
		senderPubKey = hex.EncodeToString(
			summary.SenderPubkey.SerializeCompressed(),
		)
	}

	var preimage string
	if summary.Preimage != nil {
		preimage = hex.EncodeToString(summary.Preimage[:])
	}

	return &swapclientrpc.SwapSummary{
		Direction:        swapDirectionToProto(summary.Direction),
		PaymentHash:      hex.EncodeToString(summary.PaymentHash[:]),
		Invoice:          summary.Invoice,
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
		SettlementType: swapSettlementTypeToProto(
			summary.SettlementType,
		),
		SenderPubkey:       senderPubKey,
		Preimage:           preimage,
		CreditQuote:        creditQuoteToProto(summary.CreditQuote),
		RequestedAmountSat: summary.RequestedAmountSat,
		AttachedCreditSat:  summary.AttachedCreditSat,
		DustLimitSat:       summary.DustLimitSat,
		AvailableCreditSat: summary.AvailableCreditSat,
	}
}

func quotePayToProto(quote *swaps.InSwapQuote) (*swapclientrpc.QuotePayResponse,
	error) {

	if quote == nil {
		return nil, status.Error(
			codes.Internal, "quote pay response is empty",
		)
	}

	return &swapclientrpc.QuotePayResponse{
		PaymentHash:      hex.EncodeToString(quote.PaymentHash[:]),
		InvoiceAmountSat: quote.InvoiceAmountSat,
		AmountSat:        quote.AmountSat,
		FeeSat:           quote.FeeSat,
		SettlementType: swapSettlementTypeToProto(
			quote.SettlementType,
		),
		ExpiresAtUnix: quote.Expiry.Unix(),
		ExceedsMaxFee: quote.ExceedsMaxFee,
		CreditQuote:   creditQuoteToProto(quote.CreditQuote),
	}, nil
}

func creditQuoteToProto(quote *swaps.CreditQuote) *swapclientrpc.CreditQuote {
	if quote == nil {
		return nil
	}

	return &swapclientrpc.CreditQuote{
		MustUseCredit:      quote.MustUseCredit,
		CreditAppliedSat:   quote.CreditAppliedSat,
		CreditShortfallSat: quote.CreditShortfallSat,
		CreditTopupSat:     quote.CreditTopupSat,
		ArkFundingSat:      quote.ArkFundingSat,
	}
}

func creditFundingSourceFromProto(source swapclientrpc.CreditFundingSource) (
	swaps.CreditFundingSource, error) {

	switch source {
	case swapclientrpc.
		CreditFundingSource_CREDIT_FUNDING_SOURCE_LIGHTNING_RECEIVE:
		return swaps.CreditFundingLightningReceive, nil

	case swapclientrpc.CreditFundingSource_CREDIT_FUNDING_SOURCE_ARK_TOPUP:
		return swaps.CreditFundingArkTopUp, nil

	default:
		return "", status.Error(
			codes.InvalidArgument, "credit source is required",
		)
	}
}

func creditCreateResponseToProto(
	op *swaps.CreditOperation) *swapclientrpc.CreateCreditResponse {

	if op == nil {
		return nil
	}

	resp := &swapclientrpc.CreateCreditResponse{
		OperationId:       op.OperationID,
		State:             creditStateToProto(op.State),
		Invoice:           op.Invoice,
		PaymentHash:       creditPaymentHashString(op.PaymentHash),
		AmountSat:         op.AmountSat,
		DestinationPubkey: append([]byte(nil), op.DestinationKey...),
	}
	if op.ExpiresAt != nil {
		resp.ExpiresAtUnix = op.ExpiresAt.Unix()
	}

	return resp
}

func creditSnapshotToProto(
	snapshot *swaps.CreditSnapshot) *swapclientrpc.ListCreditsResponse {

	if snapshot == nil {
		return nil
	}

	resp := &swapclientrpc.ListCreditsResponse{
		FinalizedSat: snapshot.FinalizedSat,
		ReservedSat:  snapshot.ReservedSat,
		AvailableSat: snapshot.AvailableSat,
	}
	for _, op := range snapshot.Operations {
		resp.Operations = append(
			resp.Operations, creditOperationToProto(op),
		)
	}
	for _, entry := range snapshot.LedgerEntries {
		resp.LedgerEntries = append(
			resp.LedgerEntries, creditLedgerEntryToProto(entry),
		)
	}

	return resp
}

func creditOperationToProto(
	op swaps.CreditOperation) *swapclientrpc.CreditOperation {

	resp := &swapclientrpc.CreditOperation{
		OperationId:       op.OperationID,
		Type:              creditTypeToProto(op.Type),
		State:             creditStateToProto(op.State),
		AmountSat:         op.AmountSat,
		PaymentHash:       creditPaymentHashString(op.PaymentHash),
		Invoice:           op.Invoice,
		DestinationPubkey: append([]byte(nil), op.DestinationKey...),
		SessionId:         op.SessionID,
		CreatedAtUnix:     op.CreatedAt.Unix(),
		UpdatedAtUnix:     op.UpdatedAt.Unix(),
		LastError:         op.LastError,
	}
	if op.CompletedAt != nil {
		resp.CompletedAtUnix = op.CompletedAt.Unix()
	}

	return resp
}

func creditLedgerEntryToProto(
	entry swaps.CreditLedgerEntry) *swapclientrpc.CreditLedgerEntry {

	return &swapclientrpc.CreditLedgerEntry{
		EntryId:       entry.EntryID,
		OperationId:   entry.OperationID,
		Direction:     entry.Direction,
		AmountSat:     entry.AmountSat,
		CreatedAtUnix: entry.CreatedAt.Unix(),
	}
}

func creditPaymentHashString(hash *lntypes.Hash) string {
	if hash == nil {
		return ""
	}

	return hex.EncodeToString(hash[:])
}

func creditTypeToProto(
	typ swaps.CreditOperationType) swapclientrpc.CreditOperationType {

	switch typ {
	case swaps.CreditOperationFunding:
		return swapclientrpc.
			CreditOperationType_CREDIT_OPERATION_TYPE_FUNDING

	case swaps.CreditOperationPay:
		return swapclientrpc.
			CreditOperationType_CREDIT_OPERATION_TYPE_PAY

	case swaps.CreditOperationRedemption:
		return swapclientrpc.
			CreditOperationType_CREDIT_OPERATION_TYPE_REDEMPTION

	case swaps.CreditOperationReceive:
		return swapclientrpc.
			CreditOperationType_CREDIT_OPERATION_TYPE_RECEIVE

	default:
		return swapclientrpc.
			CreditOperationType_CREDIT_OPERATION_TYPE_UNSPECIFIED
	}
}

func creditStateToProto(
	state swaps.CreditOperationState) swapclientrpc.CreditOperationState {

	switch state {
	case swaps.CreditStateCreated:
		return swapclientrpc.
			CreditOperationState_CREDIT_OPERATION_STATE_CREATED

	case swaps.CreditStateAwaitingPayment:
		return swapclientrpc.
			CreditOperationState_CREDIT_OPERATION_STATE_AWAITING_PAYMENT

	case swaps.CreditStateCredited:
		return swapclientrpc.
			CreditOperationState_CREDIT_OPERATION_STATE_CREDITED

	case swaps.CreditStateReserved:
		return swapclientrpc.
			CreditOperationState_CREDIT_OPERATION_STATE_RESERVED

	case swaps.CreditStatePayingLightning:
		return swapclientrpc.
			CreditOperationState_CREDIT_OPERATION_STATE_PAYING_LIGHTNING

	case swaps.CreditStateDebited:
		return swapclientrpc.
			CreditOperationState_CREDIT_OPERATION_STATE_DEBITED

	case swaps.CreditStateSendingOOR:
		return swapclientrpc.
			CreditOperationState_CREDIT_OPERATION_STATE_SENDING_OOR

	case swaps.CreditStateRedeemed:
		return swapclientrpc.
			CreditOperationState_CREDIT_OPERATION_STATE_REDEEMED

	case swaps.CreditStateReleased:
		return swapclientrpc.
			CreditOperationState_CREDIT_OPERATION_STATE_RELEASED

	case swaps.CreditStateExpired:
		return swapclientrpc.
			CreditOperationState_CREDIT_OPERATION_STATE_EXPIRED

	case swaps.CreditStateFailed:
		return swapclientrpc.
			CreditOperationState_CREDIT_OPERATION_STATE_FAILED

	default:
		return swapclientrpc.
			CreditOperationState_CREDIT_OPERATION_STATE_UNSPECIFIED
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

// swapSettlementTypeToProto maps sdk/swaps settlement rails into the public RPC
// enum used by daemon callers.
func swapSettlementTypeToProto(
	settlementType swaps.SettlementType) swapclientrpc.SwapSettlementType {

	switch settlementType {
	case swaps.SettlementTypeLightning:
		return swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_LIGHTNING

	case swaps.SettlementTypeInArk:
		return swapclientrpc.SwapSettlementType_SWAP_SETTLEMENT_TYPE_IN_ARK

	case swaps.SettlementTypeCredit:
		return swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_CREDIT

	case swaps.SettlementTypeMixed:
		return swapclientrpc.SwapSettlementType_SWAP_SETTLEMENT_TYPE_MIXED

	default:
		return swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_UNSPECIFIED
	}
}
