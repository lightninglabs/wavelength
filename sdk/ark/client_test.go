package ark

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"io"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	btcecdsa "github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type fakeDaemonService struct {
	waverpc.UnimplementedDaemonServiceServer

	mu sync.Mutex

	infoResp              *waverpc.GetInfoResponse
	listVtxosResp         *waverpc.ListVTXOsResponse
	newReceiveResp        *waverpc.NewReceiveScriptResponse
	indexedVTXOResp       *waverpc.GetIndexedVTXOByPkScriptResponse
	expiryInfoResp        *waverpc.GetVTXOExpiryInfoResponse
	indexedOORSessionResp *waverpc.GetIndexedOORSessionByTxidResponse
	sendOORResp           *waverpc.SendOORResponse

	lastListVTXOsReq   *waverpc.ListVTXOsRequest
	lastIndexedVTXOReq *waverpc.GetIndexedVTXOByPkScriptRequest
	lastExpiryInfoReq  *waverpc.GetVTXOExpiryInfoRequest
	lastIndexedOORReq  *waverpc.GetIndexedOORSessionByTxidRequest
	lastSendOORReq     *waverpc.SendOORRequest
}

const (
	// testLiveVTXOStatus keeps the fake VTXO response readable while
	// staying within the repository's line-length limit.
	testLiveVTXOStatus = waverpc.VTXOStatus_VTXO_STATUS_LIVE

	// testSafeExpStatus keeps fake expiry info fixtures readable while
	// staying within the repository's line-length limit.
	testSafeExpStatus = waverpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_SAFE
)

var (
	// testRefExpStatus keeps fake expiry info fixtures readable while
	// staying within the repository's line-length limit.
	testRefExpStatus = testExpiryStatus(
		"VTXO_EXPIRY_STATUS_NEEDS_REFRESH")

	// testCriticalExpStatus keeps fake expiry info fixtures readable while
	// staying within the repository's line-length limit.
	testCriticalExpStatus = testExpiryStatus(
		"VTXO_EXPIRY_STATUS_CRITICAL")
)

// testExpiryStatus returns a generated VTXO expiry status by name.
func testExpiryStatus(name string) waverpc.VTXOExpiryStatus {
	return waverpc.VTXOExpiryStatus(
		waverpc.VTXOExpiryStatus_value[name],
	)
}

// watchRoundsResponse shortens the generic stream signature used by the fake
// daemon service.
type watchRoundsResponse = waverpc.WatchRoundsResponse

// testVTXOExpiryInfo builds compact fake expiry metadata for client tests.
func testVTXOExpiryInfo(status waverpc.VTXOExpiryStatus, currentHeight,
	batchExpiry, blocksRemaining int32,
	chainDepth uint32) *waverpc.VTXOExpiryInfo {

	return &waverpc.VTXOExpiryInfo{
		Status:                  status,
		CurrentHeight:           currentHeight,
		BatchExpiry:             batchExpiry,
		BlocksRemaining:         blocksRemaining,
		RefreshThresholdBlocks:  216,
		CriticalThresholdBlocks: 144,
		RelativeExpiry:          144,
		MaxTreeDepth:            0,
		ChainDepth:              chainDepth,
	}
}

var (
	// testIdentityPubKeyHex is the deterministic compressed pubkey used for
	// the fake daemon identity.
	testIdentityPubKeyHex = compressedPubKeyHex(1)

	// testOperatorPubKeyHex is the deterministic compressed pubkey used for
	// the fake operator terms.
	testOperatorPubKeyHex = compressedPubKeyHex(2)
)

// newFakeDaemonService creates a fake daemon service with deterministic
// defaults that the SDK tests can override per case.
func newFakeDaemonService() *fakeDaemonService {
	return &fakeDaemonService{
		infoResp: &waverpc.GetInfoResponse{
			Version:         "1.2.3",
			Commit:          "deadbeef",
			Network:         "regtest",
			ServerConnected: true,
			WalletType:      "btcwallet",
			WalletState: waverpc.
				WalletState_WALLET_STATE_READY,
			IdentityPubkey: testIdentityPubKeyHex,
			ServerInfo: &waverpc.ServerInfo{
				OperatorPubkey: mustDecodeHex(
					testOperatorPubKeyHex,
				),
				BoardingExitDelay: 144,
				VtxoExitDelay:     288,
				DustLimit:         546,
				MinVtxoAmountSat:  1234,
				MinBoardingAmount: 10_000,
				MaxVtxoAmount:     20_000,
				FeeRate:           15,
				MinOperatorFee:    20,
				MinConfirmations:  2,
			},
		},
		listVtxosResp: &waverpc.ListVTXOsResponse{
			Vtxos: []*waverpc.VTXO{
				{
					Outpoint:  "txid:0",
					AmountSat: 1234,
					Status:    testLiveVTXOStatus,
					PkScript:  "5120c0ffee",
					OorFinalCheckpointPsbts: [][]byte{
						{
							0x01,
							0x02,
						},
					},
					SpentByTxid: "spent-txid",
					ExpiryInfo: testVTXOExpiryInfo(
						testSafeExpStatus, 700, 1000,
						300, 0,
					),
				},
			},
		},
		newReceiveResp: &waverpc.NewReceiveScriptResponse{
			PkScriptHex: "5120c0ffee",
			PubkeyXonlyHex: "11111111111111111111111111111111" +
				"11111111111111111111111111111111",
			KeyFamily: 23,
			KeyIndex:  7,
			Label:     "receive-label",
		},
		indexedVTXOResp: &waverpc.GetIndexedVTXOByPkScriptResponse{
			Vtxo: &waverpc.VTXO{
				Outpoint:  "indexed:1",
				AmountSat: 42,
				Status: waverpc.
					VTXOStatus_VTXO_STATUS_SPENT,
				PkScript: "5120c0ffee",
				OorFinalCheckpointPsbts: [][]byte{
					{
						0xaa,
					},
				},
				SpentByTxid: "indexed-spender",
				ExpiryInfo: testVTXOExpiryInfo(
					testRefExpStatus, 784, 1000, 216, 1,
				),
			},
		},
		expiryInfoResp: &waverpc.GetVTXOExpiryInfoResponse{
			Found: true,
			ExpiryInfo: testVTXOExpiryInfo(
				testCriticalExpStatus, 900, 1000, 100, 2,
			),
			Vtxo: &waverpc.VTXO{
				Outpoint:  "expiry:0",
				AmountSat: 21_000,
				Status:    testLiveVTXOStatus,
				PkScript:  "5120c0ffee",
			},
		},
		indexedOORSessionResp: &waverpc.
			GetIndexedOORSessionByTxidResponse{
			ArkPsbt: []byte{
				0x01,
				0x02,
				0x03,
			},
			CheckpointPsbts: [][]byte{
				{
					0x04,
				},
				{
					0x05,
				},
			},
		},
		sendOORResp: &waverpc.SendOORResponse{
			SessionId: "session-123",
			RecipientOutpoints: []string{
				"session-123:0",
			},
		},
	}
}

// mustDecodeHex decodes a hex string for deterministic test fixtures.
func mustDecodeHex(value string) []byte {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		panic(err)
	}

	return decoded
}

// compressedPubKeyHex deterministically derives one compressed public key hex
// string from a tiny scalar for test fixtures.
func compressedPubKeyHex(scalar byte) string {
	privKeyBytes := make([]byte, 32)
	privKeyBytes[len(privKeyBytes)-1] = scalar

	_, pubKey := btcec.PrivKeyFromBytes(privKeyBytes)

	return hex.EncodeToString(pubKey.SerializeCompressed())
}

// fakeReceiveAuthPrivKey returns the deterministic key used by fake daemon
// receive-auth operations.
func fakeReceiveAuthPrivKey() *btcec.PrivateKey {
	privKeyBytes := make([]byte, 32)
	privKeyBytes[len(privKeyBytes)-1] = 4

	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes)

	return privKey
}

// startFakeDaemonServer boots a fake daemon gRPC server and returns the
// listener address that tests can dial through the SDK facade.
func startFakeDaemonServer(t *testing.T,
	serverOpts ...grpc.ServerOption) string {

	return startFakeDaemonServerWithService(
		t, newFakeDaemonService(), serverOpts...,
	)
}

// startFakeDaemonServerWithService boots a fake daemon gRPC server using the
// supplied service implementation and returns the listener address that tests
// can dial through the SDK facade.
func startFakeDaemonServerWithService(t *testing.T,
	service waverpc.DaemonServiceServer,
	serverOpts ...grpc.ServerOption) string {

	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := grpc.NewServer(serverOpts...)
	waverpc.RegisterDaemonServiceServer(server, service)

	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Serve(listener)
	}()

	t.Cleanup(func() {
		server.Stop()
		<-errChan
	})

	return listener.Addr().String()
}

// newLoopbackTLSCreds creates matching server and client transport
// credentials for a loopback-only TLS test server.
func newLoopbackTLSCreds(t *testing.T) (credentials.TransportCredentials,
	credentials.TransportCredentials) {

	t.Helper()

	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serialNumber, err := rand.Int(
		rand.Reader,
		new(
			big.Int).Lsh(big.NewInt(1), 128),
	)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "127.0.0.1",
		},
		NotBefore: time.Now().Add(-1 * time.Hour),
		NotAfter:  time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature |
			x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
		},
		DNSNames: []string{
			"localhost",
		},
	}

	certDER, err := x509.CreateCertificate(
		rand.Reader, template, template, &privKey.PublicKey, privKey,
	)
	require.NoError(t, err)

	cert := tls.Certificate{
		Certificate: [][]byte{
			certDER,
		},
		PrivateKey: privKey,
	}
	serverCreds := credentials.NewServerTLSFromCert(&cert)

	pool := x509.NewCertPool()
	parsedCert, err := x509.ParseCertificate(certDER)
	require.NoError(t, err)
	pool.AddCert(parsedCert)

	clientCreds := credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		ServerName: "127.0.0.1",
		MinVersion: tls.VersionTLS12,
	})

	return serverCreds, clientCreds
}

// GetInfo returns a fixed response so the remote-client test can assert the
// SDK's proto-to-typed-model conversion.
func (f *fakeDaemonService) GetInfo(context.Context, *waverpc.GetInfoRequest) (
	*waverpc.GetInfoResponse, error) {

	return f.infoResp, nil
}

// GetBalance returns a fixed balance response so the SDK wrapper can be
// exercised against a remote daemon transport.
func (f *fakeDaemonService) GetBalance(context.Context,
	*waverpc.GetBalanceRequest) (*waverpc.GetBalanceResponse, error) {

	return &waverpc.GetBalanceResponse{
		BoardingConfirmedSat:   111,
		BoardingUnconfirmedSat: 222,
		VtxoBalanceSat:         333,
		TotalConfirmedSat:      444,
	}, nil
}

// ListVTXOs returns one fixed VTXO so the SDK's thin passthrough wrapper can
// be exercised without needing a real daemon database.
func (f *fakeDaemonService) ListVTXOs(_ context.Context,
	req *waverpc.ListVTXOsRequest) (*waverpc.ListVTXOsResponse, error) {

	f.mu.Lock()
	f.lastListVTXOsReq = req
	f.mu.Unlock()

	return f.listVtxosResp, nil
}

// NewAddress returns a fixed boarding address for SDK facade testing.
func (f *fakeDaemonService) NewAddress(context.Context,
	*waverpc.NewAddressRequest) (*waverpc.NewAddressResponse, error) {

	return &waverpc.NewAddressResponse{
		Address: "bcrt1ptestaddress",
	}, nil
}

// NewReceiveScript returns one deterministic receive script so the SDK
// can verify its typed receive-script helper.
func (f *fakeDaemonService) NewReceiveScript(_ context.Context,
	_ *waverpc.NewReceiveScriptRequest) (*waverpc.NewReceiveScriptResponse,
	error) {

	return f.newReceiveResp, nil
}

// ReceiveAuthKey returns one deterministic receive-auth public key.
func (f *fakeDaemonService) ReceiveAuthKey(context.Context,
	*waverpc.ReceiveAuthKeyRequest) (*waverpc.ReceiveAuthKeyResponse,
	error) {

	return &waverpc.ReceiveAuthKeyResponse{
		Pubkey: fakeReceiveAuthPrivKey().PubKey().
			SerializeCompressed(),
	}, nil
}

// SignReceiveAuthMessage signs with the deterministic receive-auth key.
func (f *fakeDaemonService) SignReceiveAuthMessage(_ context.Context,
	req *waverpc.SignReceiveAuthMessageRequest) (
	*waverpc.SignReceiveAuthMessageResponse, error) {

	digest := chainhash.HashB(req.GetMessage())
	if req.GetDoubleHash() {
		digest = chainhash.DoubleHashB(req.GetMessage())
	}

	sig := btcecdsa.Sign(fakeReceiveAuthPrivKey(), digest)

	return &waverpc.SignReceiveAuthMessageResponse{
		Signature: sig.Serialize(),
	}, nil
}

// SignReceiveAuthMessageCompact signs with the deterministic receive-auth key
// and returns the compact signature.
func (f *fakeDaemonService) SignReceiveAuthMessageCompact(_ context.Context,
	req *waverpc.SignReceiveAuthMessageCompactRequest) (
	*waverpc.SignReceiveAuthMessageCompactResponse, error) {

	digest := chainhash.HashB(req.GetMessage())
	if req.GetDoubleHash() {
		digest = chainhash.DoubleHashB(req.GetMessage())
	}

	sig := btcecdsa.SignCompact(
		fakeReceiveAuthPrivKey(), digest, true,
	)

	return &waverpc.SignReceiveAuthMessageCompactResponse{
		Signature: sig,
	}, nil
}

// ReceiveAuthECDH returns one deterministic shared secret for facade tests.
func (f *fakeDaemonService) ReceiveAuthECDH(context.Context,
	*waverpc.ReceiveAuthECDHRequest) (*waverpc.ReceiveAuthECDHResponse,
	error) {

	return &waverpc.ReceiveAuthECDHResponse{
		SharedSecret: bytes.Repeat([]byte{0x55}, 32),
	}, nil
}

// GetIndexedVTXOByPkScript returns one deterministic indexed VTXO and records
// the lookup request for helper assertions.
func (f *fakeDaemonService) GetIndexedVTXOByPkScript(_ context.Context,
	req *waverpc.GetIndexedVTXOByPkScriptRequest) (
	*waverpc.GetIndexedVTXOByPkScriptResponse, error) {

	f.mu.Lock()
	f.lastIndexedVTXOReq = req
	f.mu.Unlock()

	return f.indexedVTXOResp, nil
}

// GetVTXOExpiryInfo returns one deterministic expiry classification and
// records the lookup request for helper assertions.
func (f *fakeDaemonService) GetVTXOExpiryInfo(_ context.Context,
	req *waverpc.GetVTXOExpiryInfoRequest) (
	*waverpc.GetVTXOExpiryInfoResponse, error) {

	f.mu.Lock()
	f.lastExpiryInfoReq = req
	f.mu.Unlock()

	return f.expiryInfoResp, nil
}

// GetIndexedOORSessionByTxid returns one deterministic indexed OOR session
// and records the lookup request for helper assertions.
func (f *fakeDaemonService) GetIndexedOORSessionByTxid(_ context.Context,
	req *waverpc.GetIndexedOORSessionByTxidRequest) (
	*waverpc.GetIndexedOORSessionByTxidResponse, error) {

	f.mu.Lock()
	f.lastIndexedOORReq = req
	f.mu.Unlock()

	return f.indexedOORSessionResp, nil
}

// SendOOR records the submitted OOR request and returns one deterministic
// session id so the helper methods can assert their request translation.
func (f *fakeDaemonService) SendOOR(_ context.Context,
	req *waverpc.SendOORRequest) (*waverpc.SendOORResponse, error) {

	f.mu.Lock()
	f.lastSendOORReq = req
	f.mu.Unlock()

	return f.sendOORResp, nil
}

// Board returns a fixed status so the SDK can verify the board wrapper against
// a remote daemon.
func (f *fakeDaemonService) Board(context.Context, *waverpc.BoardRequest) (
	*waverpc.BoardResponse, error) {

	return &waverpc.BoardResponse{Status: "registered"}, nil
}

// EstimateFee returns a fixed fee quote for the SDK facade test.
func (f *fakeDaemonService) EstimateFee(context.Context,
	*waverpc.EstimateFeeRequest) (*waverpc.EstimateFeeResponse, error) {

	return &waverpc.EstimateFeeResponse{
		TotalFeeSat:         77,
		BelowDustWarning:    false,
		MinViableAmountSat:  1000,
		EffectiveAnnualRate: 0.5,
	}, nil
}

// GetFeeHistory returns one fixed ledger entry for the SDK facade test.
func (f *fakeDaemonService) GetFeeHistory(context.Context,
	*waverpc.GetFeeHistoryRequest) (*waverpc.GetFeeHistoryResponse, error) {

	return &waverpc.GetFeeHistoryResponse{
		Entries: []*waverpc.FeeHistoryEntry{
			{
				EntryId:       1,
				EventType:     "vtxo_sent",
				AmountSat:     55,
				DebitAccount:  "transfers_out",
				CreditAccount: "vtxo_balance",
			},
		},
		TotalFeesPaidSat: 999,
	}, nil
}

// WatchRounds sends one synthetic round update and then closes the stream so
// the SDK can verify its streaming wrapper.
func (f *fakeDaemonService) WatchRounds(_ *waverpc.WatchRoundsRequest,
	stream grpc.ServerStreamingServer[watchRoundsResponse]) error {

	if err := stream.Send(&waverpc.WatchRoundsResponse{
		Round: &waverpc.RoundInfo{
			RoundId: "round-1",
			State:   waverpc.RoundState_ROUND_STATE_JOINED,
		},
	}); err != nil {
		return err
	}

	return nil
}

// TestDialRemoteGetInfo verifies the SDK can talk to a standalone daemon
// endpoint and convert the protobuf GetInfo response into the typed SDK model.
func TestDialRemoteGetInfo(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := DialRemote(ctx, RemoteConfig{
		Address:       startFakeDaemonServer(t),
		AllowInsecure: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	info, err := client.GetInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, "1.2.3", info.Version)
	require.True(t, info.ServerConnected)
	require.NotNil(t, info.ServerInfo)
	require.Equal(
		t, mustDecodeHex(testOperatorPubKeyHex),
		info.ServerInfo.OperatorPubKey,
	)
	require.Equal(t, uint32(144),
		info.ServerInfo.BoardingExitDelay,
	)
	require.Equal(t, uint64(1234),
		info.ServerInfo.MinVTXOAmountSat,
	)
	require.Equal(t, uint64(20),
		info.ServerInfo.MinOperatorFee,
	)
}

// TestDialRemoteCoversFacadeMethods verifies the thin SDK wrappers beyond
// GetInfo by exercising representative unary and streaming daemon RPCs.
func TestDialRemoteCoversFacadeMethods(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := DialRemote(ctx, RemoteConfig{
		Address:       startFakeDaemonServer(t),
		AllowInsecure: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	balance, err := client.GetBalance(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(444), balance.TotalConfirmedSat)

	vtxos, err := client.ListVTXOs(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, vtxos.Vtxos, 1)

	address, err := client.NewAddress(context.Background())
	require.NoError(t, err)
	require.Equal(t, "bcrt1ptestaddress", address.Address)

	boardResp, err := client.Board(context.Background())
	require.NoError(t, err)
	require.Equal(t, "registered", boardResp.Status)

	feeResp, err := client.EstimateFee(
		context.Background(), &waverpc.EstimateFeeRequest{
			AmountSat: 10_000,
		},
	)
	require.NoError(t, err)
	require.Equal(t, int64(77), feeResp.TotalFeeSat)

	history, err := client.GetFeeHistory(
		context.Background(), &waverpc.GetFeeHistoryRequest{
			Limit: 10,
		},
	)
	require.NoError(t, err)
	require.Len(t, history.Entries, 1)

	stream, err := client.WatchRounds(context.Background())
	require.NoError(t, err)

	update, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, "round-1", update.Round.RoundId)

	_, err = stream.Recv()
	require.ErrorIs(t, err, io.EOF)
}

// TestDialRemotePolicyHelpers verifies the higher-level policy and indexer
// helpers that sdk/swaps will depend on, so those adapters can live in
// sdk/ark rather than being reimplemented in the swap layer.
func TestDialRemotePolicyHelpers(t *testing.T) {
	t.Parallel()

	service := newFakeDaemonService()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := DialRemote(ctx, RemoteConfig{
		Address:       startFakeDaemonServerWithService(t, service),
		AllowInsecure: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	identityPubKey, err := client.IdentityPubKey(context.Background())
	require.NoError(t, err)
	require.Equal(
		t, testIdentityPubKeyHex,
		hex.EncodeToString(
			identityPubKey.SerializeCompressed(),
		),
	)

	operatorPubKey, err := client.OperatorPubKey(context.Background())
	require.NoError(t, err)
	require.Equal(
		t, testOperatorPubKeyHex,
		hex.EncodeToString(
			operatorPubKey.SerializeCompressed(),
		),
	)

	sessionID, err := client.SendOORWithPolicy(
		context.Background(), 42_000, []byte{0xaa, 0xbb},
	)
	require.NoError(t, err)
	require.Equal(t, "session-123", sessionID)

	oorResult, err := client.SendOORWithPolicyDetails(
		context.Background(), 42_000, []byte{0xaa, 0xbb},
	)
	require.NoError(t, err)
	require.Equal(t, "session-123", oorResult.SessionID)
	require.Equal(t, "session-123:0", oorResult.RecipientOutpoint)

	service.mu.Lock()
	policyReq := service.lastSendOORReq
	service.mu.Unlock()

	require.NotNil(t, policyReq)
	require.Len(t, policyReq.GetRecipients(), 1)
	require.Equal(
		t, int64(42_000), policyReq.GetRecipients()[0].GetAmountSat(),
	)
	require.Equal(
		t, []byte{0xaa, 0xbb},
		policyReq.GetRecipients()[0].GetPolicyTemplate(),
	)

	sessionID, err = client.SendOORWithCustomInputs(
		context.Background(), []byte{0x11, 0x22}, 21_000,
		[]CustomOORInput{
			{
				Outpoint:           "custom:0",
				VTXOPolicyTemplate: []byte{0x01},
				SpendPath:          []byte{0x02},
				AmountSat:          21_000,
				PkScript:           []byte{0x51},
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "session-123", sessionID)

	service.mu.Lock()
	customReq := service.lastSendOORReq
	service.mu.Unlock()

	require.NotNil(t, customReq)
	require.Len(t, customReq.GetRecipients(), 1)
	require.Equal(
		t, []byte{0x11, 0x22}, customReq.GetRecipients()[0].GetPubkey(),
	)
	require.Len(t, customReq.GetCustomInputs(), 1)
	require.Equal(
		t, "custom:0", customReq.GetCustomInputs()[0].GetOutpoint(),
	)

	liveVTXOs, err := client.ListLiveVTXOs(context.Background())
	require.NoError(t, err)
	require.Len(t, liveVTXOs, 1)
	require.Equal(
		t, []byte{0x51, 0x20, 0xc0, 0xff, 0xee}, liveVTXOs[0].PkScript,
	)
	require.NotNil(t, liveVTXOs[0].ExpiryInfo)
	require.Equal(t, testSafeExpStatus, liveVTXOs[0].ExpiryInfo.Status)
	require.Equal(t, int32(300), liveVTXOs[0].ExpiryInfo.BlocksRemaining)
	require.Equal(
		t, int32(216), liveVTXOs[0].ExpiryInfo.RefreshThresholdBlocks,
	)

	service.mu.Lock()
	listReq := service.lastListVTXOsReq
	service.mu.Unlock()

	require.NotNil(t, listReq)
	require.Equal(
		t, waverpc.VTXOStatus_VTXO_STATUS_LIVE,
		listReq.GetStatusFilter(),
	)

	indexedVTXO, err := client.FindSpentVTXOByPkScript(
		context.Background(), []byte{0x51, 0x20, 0xc0, 0xff, 0xee},
	)
	require.NoError(t, err)
	require.NotNil(t, indexedVTXO)
	require.Equal(t, "indexed:1", indexedVTXO.Outpoint)
	require.NotNil(t, indexedVTXO.ExpiryInfo)
	require.Equal(
		t, testRefExpStatus, indexedVTXO.ExpiryInfo.Status,
	)
	require.Equal(t, uint32(1), indexedVTXO.ExpiryInfo.ChainDepth)

	service.mu.Lock()
	indexedReq := service.lastIndexedVTXOReq
	service.mu.Unlock()

	require.NotNil(t, indexedReq)
	require.Equal(
		t, []byte{0x51, 0x20, 0xc0, 0xff, 0xee},
		indexedReq.GetPkScript(),
	)
	require.Equal(t, []waverpc.VTXOStatus{
		waverpc.VTXOStatus_VTXO_STATUS_SPENT,
	}, indexedReq.GetStatusFilter())

	expiryResp, err := client.GetVTXOExpiryInfo(
		context.Background(),
		&waverpc.GetVTXOExpiryInfoRequest{
			Target: &waverpc.GetVTXOExpiryInfoRequest_PkScript{
				PkScript: []byte{0x51, 0x20, 0xc0, 0xff, 0xee},
			},
			StatusFilter: []waverpc.VTXOStatus{
				waverpc.VTXOStatus_VTXO_STATUS_LIVE,
			},
			CurrentHeight: 900,
		},
	)
	require.NoError(t, err)
	require.True(t, expiryResp.GetFound())
	require.Equal(
		t, testCriticalExpStatus,
		expiryResp.GetExpiryInfo().GetStatus(),
	)

	service.mu.Lock()
	expiryReq := service.lastExpiryInfoReq
	service.mu.Unlock()

	require.NotNil(t, expiryReq)
	require.Equal(t, int32(900), expiryReq.GetCurrentHeight())
	require.Equal(
		t, []byte{0x51, 0x20, 0xc0, 0xff, 0xee},
		expiryReq.GetPkScript(),
	)

	receiveInfo, err := client.AllocateReceiveScript(
		context.Background(),
		"receive-label",
	)
	require.NoError(t, err)
	require.Equal(
		t, []byte{0x51, 0x20, 0xc0, 0xff, 0xee}, receiveInfo.PkScript,
	)
	require.Equal(t, uint32(23), receiveInfo.KeyFamily)

	authPubKey, err := client.ReceiveAuthKey(
		context.Background(), lntypes.Hash{1, 2, 3},
	)
	require.NoError(t, err)
	require.True(t, authPubKey.IsEqual(fakeReceiveAuthPrivKey().PubKey()))

	authSig, err := client.SignReceiveAuthMessage(
		context.Background(), lntypes.Hash{1, 2, 3}, []byte("msg"),
		false,
	)
	require.NoError(t, err)
	require.True(
		t,
		authSig.Verify(
			chainhash.HashB(
				[]byte("msg"),
			),
			authPubKey,
		),
	)

	compactSig, err := client.SignReceiveAuthMessageCompact(
		context.Background(), lntypes.Hash{1, 2, 3}, []byte("msg"),
		true,
	)
	require.NoError(t, err)
	require.NotEmpty(t, compactSig)

	remotePriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	sharedSecret, err := client.ReceiveAuthECDH(
		context.Background(), lntypes.Hash{1, 2, 3},
		remotePriv.PubKey(),
	)
	require.NoError(t, err)
	require.Equal(t, bytes.Repeat([]byte{0x55}, 32), sharedSecret[:])

	const sessionTxID = "11111111111111111111111111111111" +
		"11111111111111111111111111111111"

	session, err := client.GetIndexedOORSession(
		context.Background(), []byte{0x51}, sessionTxID,
	)
	require.NoError(t, err)
	require.Equal(t, []byte{0x01, 0x02, 0x03}, session.ArkPSBT)
	require.Len(t, session.CheckpointPSBTs, 2)

	service.mu.Lock()
	indexedOORReq := service.lastIndexedOORReq
	service.mu.Unlock()

	require.NotNil(t, indexedOORReq)
	require.Equal(t, []byte{0x51}, indexedOORReq.GetPkScript())
	expectedHash, err := chainhash.NewHashFromStr(sessionTxID)
	require.NoError(t, err)
	require.Equal(t, expectedHash[:], indexedOORReq.GetSessionTxid())
}

// TestDialRemoteRequiresExplicitInsecureOptIn verifies that remote dialers
// must either provide TLS credentials or explicitly allow plaintext
// transport.
func TestDialRemoteRequiresExplicitInsecureOptIn(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := DialRemote(ctx, RemoteConfig{
		Address: startFakeDaemonServer(t),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "credentials are required")
}

// TestDialRemoteTLSGetInfo verifies the remote SDK path works with explicit
// TLS credentials instead of development-only insecure transport.
func TestDialRemoteTLSGetInfo(t *testing.T) {
	t.Parallel()

	serverCreds, clientCreds := newLoopbackTLSCreds(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := DialRemote(ctx, RemoteConfig{
		Address: startFakeDaemonServer(
			t, grpc.Creds(serverCreds),
		),
		Credentials: clientCreds,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	info, err := client.GetInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, "1.2.3", info.Version)
}

// TestStartEmbeddedUsesBufconnTransport verifies the SDK can boot an embedded
// daemon, call pre-wallet RPCs, and leave the caller's config unmodified.
func TestStartEmbeddedUsesBufconnTransport(t *testing.T) {
	t.Parallel()

	cfg := waved.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.Network = "regtest"
	cfg.Server.Host = "127.0.0.1:10010"
	cfg.Wallet.Type = waved.WalletTypeBtcwallet
	cfg.Wallet.FeeURL = "http://127.0.0.1:3001"
	cfg.Wallet.EsploraURL = ""
	cfg.RPC.Gateway.ListenAddr = "127.0.0.1:0"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := StartEmbedded(ctx, EmbeddedConfig{
		DaemonConfig: cfg,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	info, err := client.GetInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, "regtest", info.Network)
	require.Equal(t, waved.WalletTypeBtcwallet, info.WalletType)
	require.False(t, info.WalletReady())

	seed, err := client.GenSeed(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, seed.Mnemonic, 24)

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- <-client.Wait()
	}()

	require.NoError(t, client.Close())
	require.NoError(t, <-waitErr)

	require.Nil(t, cfg.RPC.Listener)
}

// TestStartEmbeddedFailsFastOnBootError verifies embedded startup returns the
// daemon boot error promptly instead of waiting for the caller's readiness
// timeout when the daemon exits during startup.
func TestStartEmbeddedFailsFastOnBootError(t *testing.T) {
	t.Parallel()

	cfg := waved.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.Network = "regtest"
	cfg.Server.Host = "127.0.0.1:10010"
	cfg.Wallet.Type = "bogus"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := StartEmbedded(ctx, EmbeddedConfig{
		DaemonConfig: cfg,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, `unknown wallet type "bogus"`)
	require.Less(t, time.Since(start), time.Second)
}

// TestClientCloseIsIdempotent verifies repeated Close calls only run the
// underlying shutdown path once.
func TestClientCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	var closeCalls atomic.Int32
	client := &Client{
		closeFn: func(context.Context) error {
			closeCalls.Add(1)

			return nil
		},
	}

	require.NoError(t, client.Close())
	require.NoError(t, client.Close())
	require.Equal(t, int32(1), closeCalls.Load())
}

// TestWrapDaemonClientUsesExistingClient verifies the SDK can wrap an already
// constructed daemon client without introducing its own runtime supervision.
func TestWrapDaemonClientUsesExistingClient(t *testing.T) {
	t.Parallel()

	serverAddr := startFakeDaemonServer(t)

	conn, err := grpc.NewClient(
		serverAddr,
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
	})

	daemonClient := waverpc.NewDaemonServiceClient(conn)

	var closeCalls atomic.Int32
	client := WrapDaemonClient(daemonClient, func(context.Context) error {
		closeCalls.Add(1)

		return nil
	})

	info, err := client.GetInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, "1.2.3", info.Version)

	select {
	case _, ok := <-client.Wait():
		require.False(t, ok)

	default:
		t.Fatal("expected wrapped client wait channel to be closed")
	}

	require.NoError(t, client.Close())
	require.Equal(t, int32(1), closeCalls.Load())
}

// TestWrapDaemonServerUsesBufconnTransport verifies the SDK can wrap an
// already-running daemon RPC implementation without dialing the daemon's public
// network listener.
func TestWrapDaemonServerUsesBufconnTransport(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := WrapDaemonServer(ctx, InProcessConfig{
		DaemonServer: newFakeDaemonService(),
	})
	require.NoError(t, err)

	info, err := client.GetInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, "1.2.3", info.Version)

	require.NoError(t, client.Close())

	select {
	case err, ok := <-client.Wait():
		require.True(t, ok)
		require.NoError(t, err)

	case <-ctx.Done():
		t.Fatalf("wait for in-process daemon transport: %v", ctx.Err())
	}
}

// TestWrapDaemonServerRequiresDaemonServer verifies callers receive a clear
// setup error rather than a nil pointer panic when no in-process daemon RPC
// implementation is supplied.
func TestWrapDaemonServerRequiresDaemonServer(t *testing.T) {
	t.Parallel()

	_, err := WrapDaemonServer(context.Background(), InProcessConfig{})
	require.ErrorContains(t, err, "daemon server is required")
}
