//go:build systest

package systest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/rpc/oorpb"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	// testOperatorFeeSat is the fake operator fee returned by the mailbox
	// server and therefore charged by SendVTXO during the test.
	testOperatorFeeSat = int64(1000)

	// testRecipientAmountSat is the directed-send amount used by the test.
	testRecipientAmountSat = int64(40000)

	// testSeededAmountSat is the value of the preseeded live VTXO.
	testSeededAmountSat = int64(100000)

	// testDustLimitSat is the fake operator dust limit used by the test.
	testDustLimitSat = int64(546)

	// systestLNDTransportEnv is the environment variable that selects the
	// lnd wallet transport for every daemon built by
	// newDirectedSendFixture. When set to the REST transport value it
	// defaults the whole directed-send/OOR suite onto the lnd REST backend
	// (a per-test config mutator still overrides it), so CI can rerun the
	// signing-capable send/OOR flows against REST-backed lnd without
	// duplicating the test bodies. Unset (or "grpc") keeps the historical
	// gRPC path, so local `make systest` behaviour is unchanged.
	systestLNDTransportEnv = "ARK_SYSTEST_LND_TRANSPORT"
)

// fakeMailboxServer implements just enough of the operator mailbox edge for the
// daemon systest. It serves ArkService.GetInfo over both direct gRPC and
// mailbox unary RPC, records round JoinRound requests, and long-polls
// empty inboxes so the ingress loop does not busy-spin.
type fakeMailboxServer struct {
	mailboxpb.UnimplementedMailboxServiceServer
	arkrpc.UnimplementedArkServiceServer

	t                *testing.T
	operatorMailbox  string
	operatorInfoResp *arkrpc.GetInfoResponse

	mu              sync.Mutex
	mailboxes       map[string][]*mailboxpb.Envelope
	nextEventSeq    map[string]uint64
	joinRoundReqs   []*roundpb.JoinRoundRequest
	joinRoundEnvs   []*mailboxpb.Envelope
	oorSubmitReqs   []*oorpb.SubmitPackageRequest
	oorSubmitEnvs   []*mailboxpb.Envelope
	inboxSignalChan chan struct{}

	// failRoundOnJoin, when set, makes the operator push this
	// ClientRoundFailedResp back to the client immediately on every
	// JoinRound, simulating an operator that admits the registration but
	// cannot seal the round (e.g. it cannot fund the commitment tx).
	failRoundOnJoin *roundpb.ClientRoundFailedResp
}

// newFakeMailboxServer constructs a fake mailbox edge with the given operator
// mailbox ID and Ark GetInfo response.
func newFakeMailboxServer(t *testing.T, operatorMailbox string,
	operatorInfoResp *arkrpc.GetInfoResponse) *fakeMailboxServer {

	t.Helper()

	return &fakeMailboxServer{
		t:                t,
		operatorMailbox:  operatorMailbox,
		operatorInfoResp: operatorInfoResp,
		mailboxes:        make(map[string][]*mailboxpb.Envelope),
		nextEventSeq:     make(map[string]uint64),
		inboxSignalChan:  make(chan struct{}, 1),
	}
}

// GetInfo implements arkrpc.ArkServiceServer. It returns the canned
// operator info so the client daemon can fetch the operator pubkey
// via direct gRPC before the mailbox transport starts.
func (s *fakeMailboxServer) GetInfo(_ context.Context,
	_ *arkrpc.GetInfoRequest) (*arkrpc.GetInfoResponse, error) {

	return s.operatorInfoResp, nil
}

// Send stores inbound envelopes addressed to the fake operator and synthesizes
// unary Ark GetInfo responses back to the client's mailbox when requested.
func (s *fakeMailboxServer) Send(ctx context.Context,
	req *mailboxpb.SendRequest) (*mailboxpb.SendResponse, error) {

	if req == nil || req.Envelope == nil {
		return nil, fmt.Errorf("send request missing envelope")
	}

	env, ok := proto.Clone(
		req.Envelope,
	).(*mailboxpb.Envelope)
	if !ok {
		return nil, fmt.Errorf("clone envelope: unexpected type %T",
			req.Envelope)
	}

	// Match the compound mailbox ID that the client derives
	// as CompoundMailboxID(operatorPub, clientPub). We
	// reconstruct it from the envelope's sender (the
	// client's pubkey-derived ID) and the known operator
	// mailbox.
	expectedCompound := serverconn.CompoundMailboxID(
		s.operatorMailbox, env.Sender,
	)
	if env.Recipient == expectedCompound {
		if err := s.handleOperatorEnvelope(ctx, env); err != nil {
			return nil, err
		}
	}

	return &mailboxpb.SendResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
	}, nil
}

// Pull returns queued envelopes for a mailbox starting at the requested cursor.
// When the mailbox is empty it waits up to the requested timeout so the
// daemon's ingress loop behaves like it would against a real long-poll edge.
func (s *fakeMailboxServer) Pull(ctx context.Context,
	req *mailboxpb.PullRequest) (*mailboxpb.PullResponse, error) {

	if req == nil {
		return nil, fmt.Errorf("pull request is nil")
	}

	waitTimeout := time.Duration(req.WaitTimeoutMs) * time.Millisecond

	for {
		envelopes, nextCursor := s.pullBatch(
			req.MailboxId, req.Cursor, req.MaxEnvelopes,
		)
		if len(envelopes) > 0 {
			return &mailboxpb.PullResponse{
				Status: &mailboxpb.Status{
					Ok: true,
				},
				Envelopes:  envelopes,
				NextCursor: nextCursor,
			}, nil
		}

		if waitTimeout <= 0 {
			return &mailboxpb.PullResponse{
				Status: &mailboxpb.Status{
					Ok: true,
				},
				NextCursor: req.Cursor,
			}, nil
		}

		timer := time.NewTimer(waitTimeout)
		select {
		case <-ctx.Done():
			timer.Stop()

			return nil, ctx.Err()

		case <-s.inboxSignalChan:
			if !timer.Stop() {
				<-timer.C
			}

		case <-timer.C:
			return &mailboxpb.PullResponse{
				Status: &mailboxpb.Status{
					Ok: true,
				},
				NextCursor: req.Cursor,
			}, nil
		}
	}
}

// AckUpTo drops all envelopes with event sequence lower than the requested
// cursor for the target mailbox.
func (s *fakeMailboxServer) AckUpTo(_ context.Context,
	req *mailboxpb.AckUpToRequest) (*mailboxpb.AckUpToResponse, error) {

	if req == nil {
		return nil, fmt.Errorf("ack request is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	envelopes := s.mailboxes[req.MailboxId]
	kept := envelopes[:0]
	for _, env := range envelopes {
		if env.EventSeq >= req.Cursor {
			kept = append(kept, env)
		}
	}
	s.mailboxes[req.MailboxId] = kept

	return &mailboxpb.AckUpToResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
	}, nil
}

// handleOperatorEnvelope processes envelopes addressed to the fake operator
// mailbox. The only mailbox RPC that needs a reply for this test is
// ArkService.GetInfo; JoinRound requests are recorded for later assertions.
func (s *fakeMailboxServer) handleOperatorEnvelope(ctx context.Context,
	env *mailboxpb.Envelope) error {

	if env.Rpc == nil {
		return nil
	}

	switch {
	case env.Rpc.Kind == mailboxpb.RpcMeta_KIND_REQUEST &&
		env.Rpc.Service == "arkrpc.ArkService" &&
		env.Rpc.Method == "GetInfo":
		return s.replyWithOperatorInfo(ctx, env)

	case env.Rpc.Service == roundpb.ServiceName &&
		env.Rpc.Method == roundpb.MethodJoinRound:

		if err := s.recordJoinRound(env); err != nil {
			return err
		}

		return s.maybePushRoundFailed(env)

	case env.Rpc.Service == oorpb.ServiceName &&
		env.Rpc.Method == oorpb.MethodSubmitPackage:
		return s.recordOORSubmitPackage(env)

	default:
		return nil
	}
}

// replyWithOperatorInfo enqueues a mailbox unary response for Ark GetInfo back
// to the requesting daemon mailbox.
func (s *fakeMailboxServer) replyWithOperatorInfo(ctx context.Context,
	env *mailboxpb.Envelope) error {

	body, err := anypb.New(s.operatorInfoResp)
	if err != nil {
		return fmt.Errorf("wrap operator info: %w", err)
	}

	responseEnv := &mailboxpb.Envelope{
		ProtocolVersion: env.ProtocolVersion,
		Sender:          s.operatorMailbox,
		Recipient:       env.Rpc.ReplyTo,
		CreatedAtUnixMs: time.Now().UnixMilli(),
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
			Service:       env.Rpc.Service,
			Method:        env.Rpc.Method,
			CorrelationId: env.Rpc.CorrelationId,
		},
	}

	s.enqueueEnvelope(responseEnv)

	return nil
}

// recordOORSubmitPackage decodes and stores an OOR submit-package envelope so
// systests can assert on the real mailbox payload emitted by waved.
func (s *fakeMailboxServer) recordOORSubmitPackage(
	env *mailboxpb.Envelope) error {

	if env.Body == nil {
		return fmt.Errorf("OOR submit envelope missing body")
	}

	var req oorpb.SubmitPackageRequest
	if err := env.Body.UnmarshalTo(&req); err != nil {
		return fmt.Errorf("decode OOR submit body: %w", err)
	}

	cloned, ok := proto.Clone(&req).(*oorpb.SubmitPackageRequest)
	if !ok {
		return fmt.Errorf("clone OOR submit body: unexpected type %T",
			&req)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.oorSubmitReqs = append(s.oorSubmitReqs, cloned)
	s.oorSubmitEnvs = append(s.oorSubmitEnvs, env)

	return nil
}

// recordJoinRound decodes and stores a JoinRound request envelope so the test
// can assert on the actual round registration payload sent by the daemon.
func (s *fakeMailboxServer) recordJoinRound(env *mailboxpb.Envelope) error {
	if env.Body == nil {
		return fmt.Errorf("join round envelope missing body")
	}

	var req roundpb.JoinRoundRequest
	if err := env.Body.UnmarshalTo(&req); err != nil {
		return fmt.Errorf("decode join round body: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.joinRoundReqs = append(s.joinRoundReqs, &req)
	s.joinRoundEnvs = append(s.joinRoundEnvs, env)

	return nil
}

// setFailRoundOnJoin arms the operator to push the given ClientRoundFailedResp
// back to the client on every subsequent JoinRound. Pass nil to disarm.
func (s *fakeMailboxServer) setFailRoundOnJoin(
	resp *roundpb.ClientRoundFailedResp) {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.failRoundOnJoin = resp
}

// maybePushRoundFailed pushes the armed ClientRoundFailedResp back to the
// client that just sent a JoinRound, as a KIND_EVENT round-failed push routed
// to the daemon's round mailbox (the JoinRound envelope's sender). It is a
// no-op when the operator is not armed to fail the round.
func (s *fakeMailboxServer) maybePushRoundFailed(
	joinEnv *mailboxpb.Envelope) error {

	s.mu.Lock()
	resp := s.failRoundOnJoin
	s.mu.Unlock()

	if resp == nil {
		return nil
	}

	body, err := anypb.New(resp)
	if err != nil {
		return fmt.Errorf("wrap round failed: %w", err)
	}

	s.enqueueEnvelope(&mailboxpb.Envelope{
		// Echo the JoinRound's protocol versions rather than hardcoding
		// them: the daemon's inbound validation drops any envelope
		// whose versions don't match its runtime binding, so copying
		// the versions off the daemon's own outbound envelope
		// guarantees the push survives that check. Hardcoding here
		// would silently drop the push and hang the test.
		ProtocolVersion:    joinEnv.ProtocolVersion,
		ArkProtocolVersion: joinEnv.ArkProtocolVersion,
		Sender:             s.operatorMailbox,
		Recipient:          joinEnv.Sender,
		CreatedAtUnixMs:    time.Now().UnixMilli(),
		Body:               body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_EVENT,
			Service: roundpb.ServiceName,
			Method:  roundpb.MethodRoundFailed,
			ReplyTo: s.operatorMailbox,
		},
	})

	return nil
}

// enqueueEnvelope appends an envelope to the recipient mailbox, assigning the
// next event sequence for that mailbox.
func (s *fakeMailboxServer) enqueueEnvelope(env *mailboxpb.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()

	nextSeq := s.nextEventSeq[env.Recipient] + 1
	s.nextEventSeq[env.Recipient] = nextSeq
	env.EventSeq = nextSeq

	s.mailboxes[env.Recipient] = append(s.mailboxes[env.Recipient], env)

	select {
	case s.inboxSignalChan <- struct{}{}:
	default:
	}
}

// pullBatch returns the available envelopes for a mailbox starting at the
// requested cursor along with the exclusive next cursor.
func (s *fakeMailboxServer) pullBatch(mailboxID string, cursor uint64,
	maxEnvelopes uint32) ([]*mailboxpb.Envelope, uint64) {

	s.mu.Lock()
	defer s.mu.Unlock()

	envelopes := s.mailboxes[mailboxID]
	if maxEnvelopes == 0 {
		maxEnvelopes = uint32(len(envelopes))
	}

	var batch []*mailboxpb.Envelope
	nextCursor := cursor

	for _, env := range envelopes {
		if env.EventSeq < cursor {
			continue
		}

		cloned, ok := proto.Clone(
			env,
		).(*mailboxpb.Envelope)
		if !ok {
			continue
		}

		batch = append(batch, cloned)
		nextCursor = env.EventSeq + 1

		if uint32(len(batch)) >= maxEnvelopes {
			break
		}
	}

	return batch, nextCursor
}

// oorSubmitPackages returns a cloned snapshot of recorded OOR submit requests.
func (s *fakeMailboxServer) oorSubmitPackages() []*oorpb.SubmitPackageRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	reqs := make([]*oorpb.SubmitPackageRequest, 0, len(s.oorSubmitReqs))
	for _, req := range s.oorSubmitReqs {
		cloned, ok := proto.Clone(req).(*oorpb.SubmitPackageRequest)
		if !ok {
			continue
		}

		reqs = append(reqs, cloned)
	}

	return reqs
}

// joinRoundRequests returns a defensive copy of the JoinRound requests the
// daemon has published to the fake operator mailbox so far.
func (s *fakeMailboxServer) joinRoundRequests() []*roundpb.JoinRoundRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	reqs := make([]*roundpb.JoinRoundRequest, 0, len(s.joinRoundReqs))
	for _, req := range s.joinRoundReqs {
		cloned, ok := proto.Clone(req).(*roundpb.JoinRoundRequest)
		if !ok {
			continue
		}

		reqs = append(reqs, cloned)
	}

	return reqs
}

// directedSendFixture owns the daemon under test, its fake operator edge, and
// the gRPC client used by the systest.
type directedSendFixture struct {
	t              *testing.T
	harness        *SysTestHarness
	client         waverpc.DaemonServiceClient
	conn           *grpc.ClientConn
	mailboxServer  *fakeMailboxServer
	operatorKey    *btcec.PublicKey
	seededOutpoint wire.OutPoint

	// cfg is the daemon configuration. It is retained so the daemon can be
	// torn down and relaunched against the same data directory by
	// restart().
	cfg *waved.Config

	// rpcAddr is the daemon's gRPC listen address, reused across restarts
	// so the client reconnects to the same endpoint.
	rpcAddr string

	// serverCancel cancels the currently running daemon, and serverErrChan
	// receives its run error. Both are replaced on every launch().
	serverCancel  context.CancelFunc
	serverErrChan chan error
}

// newDirectedSendFixture starts a full waved instance against the systest
// LND backend and a fake mailbox edge, then waits for the daemon RPC to become
// ready. Optional cfgMutators are applied to the daemon Config after the base
// setup (and before the DB is seeded / the server starts) so callers can tune
// timeouts and feature flags for their scenario.
func newDirectedSendFixture(t *testing.T,
	cfgMutators ...func(*waved.Config)) *directedSendFixture {

	t.Helper()

	h := NewSysTestHarness(t)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorInfo := &arkrpc.GetInfoResponse{
		Pubkey:            operatorPriv.PubKey().SerializeCompressed(),
		BoardingExitDelay: 144,
		VtxoExitDelay:     144,
		DustLimit:         testDustLimitSat,
		MinOperatorFee:    testOperatorFeeSat,
		MinConfirmations:  1,

		// The operator must advertise and select an Ark protocol
		// version, otherwise the client's bootstrap negotiation fails
		// closed with "no compatible ark protocol version". The enabled
		// versions are carried as ACTIVE policies.
		SelectedArkVersion: arkrpc.ArkProtocolVersionV1,
		ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{
			{
				Version: arkrpc.ArkProtocolVersionV1,
				State:   arkrpc.ArkVersionPolicy_STATE_ACTIVE,
			},
		},
	}

	operatorMailbox := serverconn.PubKeyMailboxID(
		operatorPriv.PubKey(),
	)
	mailboxAddr, mailboxServer, stopMailbox := startFakeMailboxServer(
		t, operatorMailbox, operatorInfo,
	)
	t.Cleanup(stopMailbox)

	dataDir := t.TempDir()
	rpcAddr := newLoopbackAddr(t)

	cfg := waved.DefaultConfig()
	cfg.DataDir = dataDir
	cfg.Network = "regtest"
	cfg.DebugLevel = "info"
	cfg.Wallet.Type = waved.WalletTypeLnd
	cfg.Lnd.Host = net.JoinHostPort("localhost", h.Harness.LNDGRPCPort)
	lndDataDir := filepath.Join(h.Harness.BaseDir(), "lnd")
	cfg.Lnd.TLSPath = filepath.Join(lndDataDir, "tls.cert")
	cfg.Lnd.MacaroonPath = filepath.Join(
		lndDataDir, "data", "chain", "bitcoin", "regtest",
		"admin.macaroon",
	)
	cfg.Server.Host = mailboxAddr
	cfg.Server.Insecure = true
	cfg.RPC.ListenAddr = rpcAddr
	cfg.RPC.Gateway.ListenAddr = newLoopbackAddr(t)
	cfg.RPC.NoTLS = true
	cfg.RPC.NoMacaroons = true

	// Default the lnd transport from the environment so a CI job can rerun
	// the signing-capable send/OOR flows over REST without a per-test
	// mutator. Applied before the mutators below so an explicit per-test
	// mutator still wins.
	if os.Getenv(systestLNDTransportEnv) == string(waved.RPCTransportREST) {
		cfg.Lnd.Transport = waved.RPCTransportREST
	}

	for _, mutate := range cfgMutators {
		mutate(cfg)
	}

	// When a mutator opts the lnd backend into the REST transport, point
	// the host at the harness lnd REST gateway port (8080) instead of the
	// gRPC port set above. The TLS cert and macaroon paths are unchanged;
	// the lndrest client reads the same files and reaches lnd's
	// grpc-gateway over HTTPS.
	if cfg.Lnd.Transport == waved.RPCTransportREST {
		cfg.Lnd.Host = net.JoinHostPort(
			"localhost", h.Harness.LNDRestPort,
		)
	}

	seededOutpoint := seedLiveVTXO(
		t, cfg, operatorPriv.PubKey(),
		btcutil.Amount(testSeededAmountSat),
	)

	fixture := &directedSendFixture{
		t:              t,
		harness:        h,
		mailboxServer:  mailboxServer,
		operatorKey:    operatorPriv.PubKey(),
		seededOutpoint: seededOutpoint,
		cfg:            cfg,
		rpcAddr:        rpcAddr,
	}

	// Register the shutdown before launching so a launch that fails its
	// readiness wait (which t.Fatals) still tears down the server
	// goroutine. shutdown is idempotent and a no-op when nothing started.
	t.Cleanup(fixture.shutdown)
	fixture.launch()

	return fixture
}

// launch starts a fresh waved instance from the fixture's retained config and
// connects a new client to it, waiting for the daemon RPC to become ready. It
// is used both for the initial start and for relaunch after restart().
func (f *directedSendFixture) launch() {
	t := f.t

	ctx, cancel := context.WithCancel(f.harness.Context())

	server, err := waved.NewServer(f.cfg)
	require.NoError(t, err)

	errChan := make(chan error, 1)
	go func() {
		errChan <- server.RunWithContext(ctx)
	}()

	f.serverCancel = cancel
	f.serverErrChan = errChan

	conn, err := grpc.NewClient(
		f.rpcAddr,
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	require.NoError(t, err)

	f.conn = conn
	f.client = waverpc.NewDaemonServiceClient(conn)

	waitForDaemonReady(t, f.client)
}

// shutdown stops the currently running daemon and closes its client connection,
// waiting for a clean exit. It is idempotent so it is safe both as the test
// cleanup and as the first half of restart().
func (f *directedSendFixture) shutdown() {
	t := f.t

	if f.conn != nil {
		require.NoError(t, f.conn.Close())
		f.conn = nil
	}

	if f.serverCancel == nil {
		return
	}

	f.serverCancel()
	select {
	case runErr := <-f.serverErrChan:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			require.NoError(t, runErr)
		}

	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for waved shutdown")
	}

	f.serverCancel = nil
}

// restart tears the daemon down and brings it back up against the same data
// directory, simulating a crash/restart. Persisted state (including VTXOs
// reserved into pending-forfeit) survives; the in-memory round FSM does not.
func (f *directedSendFixture) restart() {
	f.shutdown()
	f.launch()
}

// waitForDaemonReady polls GetInfo until the daemon reports wallet
// readiness.
func waitForDaemonReady(t *testing.T, client waverpc.DaemonServiceClient) {
	t.Helper()

	require.Eventually(
		t,
		func() bool {
			ctx, cancel := context.WithTimeout(
				t.Context(), 2*time.Second,
			)
			defer cancel()

			info, err := client.GetInfo(
				ctx, &waverpc.GetInfoRequest{},
			)
			if err != nil {
				return false
			}

			return info.GetWalletState() ==
				waverpc.WalletState_WALLET_STATE_READY
		},
		30*time.Second,
		200*time.Millisecond,
		"daemon did not become ready",
	)
}

// startFakeMailboxServer starts an in-process gRPC mailbox server and returns
// its listen address, the server implementation, and a cleanup function.
func startFakeMailboxServer(t *testing.T, operatorMailbox string,
	operatorInfoResp *arkrpc.GetInfoResponse) (string, *fakeMailboxServer,
	func()) {

	t.Helper()

	listener, err := net.Listen("tcp", newLoopbackAddr(t))
	require.NoError(t, err)

	serverImpl := newFakeMailboxServer(
		t, operatorMailbox, operatorInfoResp,
	)

	grpcServer := grpc.NewServer()
	mailboxpb.RegisterMailboxServiceServer(grpcServer, serverImpl)
	arkrpc.RegisterArkServiceServer(grpcServer, serverImpl)

	go func() {
		if serveErr := grpcServer.Serve(listener); serveErr != nil &&
			!errors.Is(serveErr, grpc.ErrServerStopped) {

			t.Errorf("fake mailbox server exited: %v", serveErr)
		}
	}()

	stopFn := func() {
		grpcServer.GracefulStop()
		err := listener.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			require.NoError(t, err)
		}
	}

	return listener.Addr().String(), serverImpl, stopFn
}

// newLoopbackAddr reserves and returns a free loopback TCP address for a test
// server to bind immediately afterward.
func newLoopbackAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	return addr
}

// seedLiveVTXO creates the daemon database ahead of startup and inserts one
// live VTXO so the manager can recover it during boot.
func seedLiveVTXO(t *testing.T, cfg *waved.Config, operatorKey *btcec.PublicKey,
	amount btcutil.Amount) wire.OutPoint {

	t.Helper()

	networkDir := cfg.NetworkDir()
	require.NoError(t, os.MkdirAll(networkDir, 0o700))

	sqliteStore, err := db.NewSqliteStore(
		db.DefaultSqliteConfig(networkDir), btclog.Disabled,
	)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, sqliteStore.Close())
	}()

	store := db.NewStore(
		sqliteStore.DB, sqliteStore.Queries, sqliteStore.Backend(),
		btclog.Disabled,
	)
	roundStore := store.NewRoundStore(
		&chaincfg.RegressionNetParams, clock.NewDefaultClock(),
	)
	vtxoStore := store.NewVTXOStore(clock.NewDefaultClock())

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	roundID, err := round.NewRoundID()
	require.NoError(t, err)

	descriptor, err := tree.NewVTXODescriptor(
		amount, clientPriv.PubKey(), operatorKey, 144,
	)
	require.NoError(t, err)

	tapScript, err := arkscript.VTXOTapScript(
		clientPriv.PubKey(), operatorKey, 144,
	)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte(t.Name() + "-seeded-vtxo")),
		Index: 0,
	}
	commitmentTxID := chainhash.HashH([]byte(t.Name() + "-commitment"))
	treePath := &tree.Tree{
		BatchOutpoint: outpoint,
		Root: &tree.Node{
			Input:     outpoint,
			Outputs:   []*wire.TxOut{},
			CoSigners: []*btcec.PublicKey{},
			Children:  make(map[uint32]*tree.Node),
		},
	}
	commitmentTx := wire.NewMsgTx(2)
	commitmentTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x11},
			Index: 0,
		},
	})
	commitmentTx.AddTxOut(&wire.TxOut{
		Value:    int64(amount),
		PkScript: descriptor.PkScript,
	})
	commitmentPSBT, err := psbt.NewFromUnsignedTx(commitmentTx)
	require.NoError(t, err)

	err = roundStore.CommitState(t.Context(), &round.Round{
		RoundID:      roundID,
		StartHeight:  1,
		CommitmentTx: fn.Some(commitmentPSBT),
		VTXOTreePaths: fn.Some(map[int]*tree.Tree{
			0: treePath,
		}),
	}, &round.InputSigSentState{
		RoundID:     roundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	})
	require.NoError(t, err)

	err = roundStore.FinalizeRound(
		t.Context(),
		roundID,
		commitmentTx.TxHash(),
		round.ConfInfo{
			Height:    1,
			BlockHash: chainhash.HashH([]byte(t.Name() + "-block")),
		},
	)
	require.NoError(t, err)

	err = vtxoStore.SaveVTXO(t.Context(), &vtxo.Descriptor{
		Outpoint:       outpoint,
		Amount:         amount,
		PolicyTemplate: descriptor.PolicyTemplate,
		PkScript:       descriptor.PkScript,
		ClientKey: keychain.KeyDescriptor{
			PubKey: clientPriv.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: types.VTXOOwnerKeyFamily,
				Index:  7,
			},
		},
		OperatorKey: operatorKey,
		TapScript:   tapScript,
		Ancestry: []types.Ancestry{{
			TreePath:       treePath,
			CommitmentTxID: commitmentTxID,
			TreeDepth:      0,
		}},
		RoundID:        roundID.String(),
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    500000,
		RelativeExpiry: 144,
		CreatedHeight:  1,
		Status:         vtxo.VTXOStatusLive,
	})
	require.NoError(t, err)

	return outpoint
}

// listAllVTXOs returns the daemon's current VTXO view via the public RPC.
func listAllVTXOs(t *testing.T,
	client waverpc.DaemonServiceClient) []*waverpc.VTXO {

	t.Helper()

	ctx, cancel := context.WithTimeout(
		t.Context(), 5*time.Second,
	)
	defer cancel()

	resp, err := client.ListVTXOs(ctx, &waverpc.ListVTXOsRequest{})
	require.NoError(t, err)

	return resp.Vtxos
}

// listRounds returns the daemon's current round state view via the public RPC.
func listRounds(t *testing.T,
	client waverpc.DaemonServiceClient) []*waverpc.RoundInfo {

	t.Helper()

	ctx, cancel := context.WithTimeout(
		t.Context(), 5*time.Second,
	)
	defer cancel()

	resp, err := client.ListRounds(ctx, &waverpc.ListRoundsRequest{})
	require.NoError(t, err)

	return resp.Rounds
}

// findVTXOByOutpoint looks up a waverpc.VTXO by its string outpoint.
func findVTXOByOutpoint(vtxos []*waverpc.VTXO,
	outpoint wire.OutPoint) *waverpc.VTXO {

	target := fmt.Sprintf("%s:%d", outpoint.Hash, outpoint.Index)
	for _, v := range vtxos {
		if v.Outpoint == target {
			return v
		}
	}

	return nil
}

// oorPolicyRecipient returns a policy-backed OOR recipient for systests that
// need a standard Ark output shape without relying on address parsing.
func oorPolicyRecipient(t *testing.T, ownerKey, operatorKey *btcec.PublicKey,
	amountSat int64) *waverpc.Output {

	t.Helper()

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		ownerKey, operatorKey, 144,
	)
	require.NoError(t, err)

	return &waverpc.Output{
		Destination: &waverpc.Output_PolicyTemplate{
			PolicyTemplate: policyTemplate,
		},
		AmountSat:          amountSat,
		VtxoPolicyTemplate: policyTemplate,
	}
}

// TestSendVTXOEndToEnd exercises directed send through the full daemon stack:
// gRPC RPC handling, wallet admission, VTXO manager state transition, round
// registration, and mailbox egress of the JoinRound request.
func TestSendVTXOEndToEnd(t *testing.T) {
	ParallelN(t)

	fixture := newDirectedSendFixture(t)

	initialVTXOs := listAllVTXOs(t, fixture.client)
	require.Len(t, initialVTXOs, 1)
	require.Equal(
		t, waverpc.VTXOStatus_VTXO_STATUS_LIVE, initialVTXOs[0].Status,
	)

	recipientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(
		t.Context(), 10*time.Second,
	)
	defer cancel()

	sendResp, err := fixture.client.SendVTXO(
		ctx, &waverpc.SendVTXORequest{
			Recipients: []*waverpc.Output{
				{
					Destination: &waverpc.Output_Pubkey{
						Pubkey: schnorr.SerializePubKey(
							recipientPriv.PubKey(),
						),
					},
					AmountSat: testRecipientAmountSat,
				},
			},
		})
	require.NoError(t, err)
	require.Equal(t, "submitted", sendResp.Status)
	require.Equal(t, testRecipientAmountSat, sendResp.TotalAmountSat)
	require.Equal(
		t,
		testSeededAmountSat-testRecipientAmountSat-testOperatorFeeSat,
		sendResp.ChangeAmountSat,
	)
	require.Equal(t, int32(1), sendResp.SelectedCount)

	require.Eventually(
		t,
		func() bool {
			vtxos := listAllVTXOs(t, fixture.client)
			vtxoInfo := findVTXOByOutpoint(
				vtxos, fixture.seededOutpoint,
			)
			if vtxoInfo == nil {
				return false
			}

			return vtxoInfo.Status ==
				waverpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT
		},
		20*time.Second,
		200*time.Millisecond,
		"seeded VTXO did not transition to pending forfeit",
	)

	statePending := waverpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY
	stateRegSent := waverpc.RoundState_ROUND_STATE_REGISTRATION_SENT

	require.Eventually(
		t,
		func() bool {
			rounds := listRounds(t, fixture.client)
			for _, roundInfo := range rounds {
				if !roundInfo.IsTemp {
					continue
				}

				switch roundInfo.State {
				case statePending, stateRegSent:
					return true

				default:
				}
			}

			return false
		},
		20*time.Second,
		200*time.Millisecond,
		"round did not reach pending-assembly state",
	)
}

// TestSendOORMultipleRecipientsEndToEnd exercises the public SendOOR RPC
// through the full daemon stack with more than one requested recipient. The
// assertion is intentionally made at the fake operator mailbox boundary: the
// RPC handler must aggregate the requested amount, the wallet must lock one
// input, the OOR actor must build one session, and the serialized
// SubmitPackage request must carry both recipient outputs in the single OOR
// package that future swap batching will rely on.
func TestSendOORMultipleRecipientsEndToEnd(t *testing.T) {
	ParallelN(t)

	fixture := newDirectedSendFixture(t)

	recipientPrivA, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientPrivB, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		amountA = int64(40_000)
		amountB = int64(60_000)
	)

	ctx, cancel := context.WithTimeout(
		t.Context(), 10*time.Second,
	)
	defer cancel()

	resp, err := fixture.client.SendOOR(
		ctx, &waverpc.SendOORRequest{
			Recipients: []*waverpc.Output{
				oorPolicyRecipient(
					t, recipientPrivA.PubKey(),
					fixture.operatorKey, amountA,
				),
				oorPolicyRecipient(
					t, recipientPrivB.PubKey(),
					fixture.operatorKey, amountB,
				),
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", resp.GetStatus())
	require.NotEmpty(t, resp.GetSessionId())
	require.Len(t, resp.GetRecipientOutpoints(), 2)

	require.Eventually(t, func() bool {
		reqs := fixture.mailboxServer.oorSubmitPackages()
		if len(reqs) != 1 {
			return false
		}

		return len(reqs[0].GetRecipientOutputs()) == 2
	}, 20*time.Second, 200*time.Millisecond)

	reqs := fixture.mailboxServer.oorSubmitPackages()
	require.Len(t, reqs, 1)

	recipientAmounts := make(map[int64]int)
	for _, recipient := range reqs[0].GetRecipientOutputs() {
		recipientAmounts[recipient.GetValueSat()]++
		require.NotEmpty(t, recipient.GetPkScript())
		require.NotEmpty(t, recipient.GetVtxoPolicyTemplate())
	}

	require.Equal(t, map[int64]int{
		amountA: 1,
		amountB: 1,
	}, recipientAmounts)
}

// TestSendOnChainSweepAllEndToEnd exercises the atomic onchain sweep-all send
// through the full daemon stack and pins the regression where the round FSM
// rejected the sweep-all intent before it ever reached the operator. The
// sweep-all leave ships IsChange=true with the pre-fee Σ(inputs) as its
// placeholder value; a bare-zero placeholder tripped the IntentRequested
// "total VTXO output is zero" guard, failing the round at PendingRoundAssembly
// and dropping every sweep-all send. The assertion is made at the fake
// operator mailbox boundary: a JoinRound request must egress, which only
// happens once the FSM clears IntentRequested and publishes the registration.
func TestSendOnChainSweepAllEndToEnd(t *testing.T) {
	ParallelN(t)

	fixture := newDirectedSendFixture(t)

	initialVTXOs := listAllVTXOs(t, fixture.client)
	require.Len(t, initialVTXOs, 1)
	require.Equal(
		t, waverpc.VTXOStatus_VTXO_STATUS_LIVE, initialVTXOs[0].Status,
	)

	// A standard P2TR destination script: OP_1 followed by a 32-byte
	// witness program. The content is irrelevant to the round FSM; only
	// its recognised script class matters to the RPC's leave-script guard.
	destPkScript := append(
		[]byte{0x51, 0x20}, make([]byte, 32)...,
	)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	sendResp, err := fixture.client.SendOnChain(
		ctx, &waverpc.SendOnChainRequest{
			Destination: &waverpc.LeaveDestination{
				Target: &waverpc.LeaveDestination_PkScript{
					PkScript: destPkScript,
				},
			},
			Amount: &waverpc.SendOnChainRequest_SweepAll{
				SweepAll: true,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", sendResp.Status)

	// Sweep-all reports the pre-fee Σ(inputs) as the actual amount; the
	// operator reduces it by the seal-time fee.
	require.Equal(t, testSeededAmountSat, sendResp.ActualAmountSat)

	// The seeded VTXO must move to pending-forfeit as the round adopts it.
	require.Eventually(
		t,
		func() bool {
			vtxos := listAllVTXOs(t, fixture.client)
			vtxoInfo := findVTXOByOutpoint(
				vtxos, fixture.seededOutpoint,
			)
			if vtxoInfo == nil {
				return false
			}

			return vtxoInfo.Status ==
				waverpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT
		},
		20*time.Second,
		200*time.Millisecond,
		"seeded VTXO did not transition to pending forfeit",
	)

	// The core regression assertion: the sweep-all intent must clear the
	// round FSM's IntentRequested validation and publish a JoinRound
	// request to the operator. Before the fix the FSM failed the round at
	// PendingRoundAssembly ("total VTXO output is zero") and nothing
	// egressed, so this Eventually would time out.
	require.Eventually(
		t,
		func() bool {
			reqs := fixture.mailboxServer.joinRoundRequests()

			return len(reqs) >= 1
		},
		20*time.Second,
		200*time.Millisecond,
		"sweep-all send did not publish a JoinRound request",
	)
}
