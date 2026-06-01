//go:build swapruntime

package swapclientserver

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/darepod"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/sdk/swaps"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestResumePendingStartsWorkersAndDedupes(t *testing.T) {
	t.Parallel()

	payHash := testHash(1)
	receiveHash := testHash(2)
	fakeClient := newFakeSwapRuntime(
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionPay,
			PaymentHash: payHash,
			State:       "funding",
			Pending:     true,
		},
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionReceive,
			PaymentHash: receiveHash,
			State:       "invoice_created",
			Pending:     true,
		},
	)
	service := newTestSwapClientService(fakeClient)
	defer service.cancel()

	service.resumePending(t.Context())
	fakeClient.awaitPayResume(t, payHash)
	fakeClient.awaitReceiveResume(t, receiveHash)

	_, err := service.ResumeSwap(
		t.Context(), &swapclientrpc.ResumeSwapRequest{
			PaymentHash: hex.EncodeToString(payHash[:]),
			Direction: swapclientrpc.
				SwapDirection_SWAP_DIRECTION_PAY,
		},
	)
	require.NoError(t, err)

	require.Equal(t, 1, fakeClient.payResumeCount(payHash))
	require.Equal(t, 1, fakeClient.receiveResumeCount(receiveHash))
	require.True(t, fakeClient.sawPendingOnlyList())
}

// TestResumeSwapConcurrentCallsStartOnePayWorker drives many manual resume RPCs
// for the same pay swap at the same time. ResumeSwap is allowed to be retried
// by clients, but it must not create parallel FSM drivers for one payment hash.
// The test starts all callers from the same barrier, verifies every RPC still
// returns successfully with the current summary, and then asserts exactly one
// ResumePayViaLightning call was admitted through the active-worker gate.
func TestResumeSwapConcurrentCallsStartOnePayWorker(t *testing.T) {
	t.Parallel()

	payHash := testHash(9)
	fakeClient := newFakeSwapRuntime(swaps.SwapSummary{
		Direction:   swaps.SwapDirectionPay,
		PaymentHash: payHash,
		State:       "waiting_for_claim",
		Pending:     true,
	})
	service := newTestSwapClientService(fakeClient)
	defer service.cancel()

	req := &swapclientrpc.ResumeSwapRequest{
		PaymentHash: hex.EncodeToString(payHash[:]),
		Direction:   swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
	}

	const callers = 16
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			<-start
			_, err := service.ResumeSwap(t.Context(), req)
			errs <- err
		}()
	}

	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	fakeClient.awaitPayResume(t, payHash)
	require.Equal(t, 1, fakeClient.payResumeCount(payHash))

	select {
	case got := <-fakeClient.payResumeCh:
		t.Fatalf("unexpected duplicate pay resume for %x", got[:])

	case <-time.After(50 * time.Millisecond):
	}
}

// TestSwapStoreDatabasePathDefaultsToNetworkDir verifies a default swap store
// is reset together with the network-scoped daemon DB directory.
func TestSwapStoreDatabasePathDefaultsToNetworkDir(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	path, err := swapStoreDatabasePath(&darepod.Config{
		DataDir: dataDir,
		Network: "signet",
	}, &darepod.SwapConfig{})
	require.NoError(t, err)
	require.Equal(
		t, filepath.Join(dataDir, "data", "signet", "swaps.db"), path,
	)
}

// TestSwapStoreDatabasePathUsesValidatedDataDir verifies the default swap store
// follows the daemon's validated data directory, including config-level tilde
// expansion.
func TestSwapStoreDatabasePathUsesValidatedDataDir(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	daemonCfg := darepod.DefaultConfig()
	daemonCfg.Network = "regtest"
	daemonCfg.Wallet.EsploraURL = "https://esplora.example/api"
	require.NoError(t, daemonCfg.Validate())

	path, err := swapStoreDatabasePath(daemonCfg, &darepod.SwapConfig{})
	require.NoError(t, err)
	require.Equal(
		t, filepath.Join(
			home, ".darepod", "data", "regtest", "swaps.db",
		),
		path,
	)
}

// TestSwapStoreDatabasePathExpandsConfiguredHome verifies explicit operator
// paths follow the same leading-tilde behavior as the daemon datadir.
func TestSwapStoreDatabasePathExpandsConfiguredHome(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	daemonCfg := darepod.DefaultConfig()
	daemonCfg.DataDir = t.TempDir()
	daemonCfg.Network = "signet"
	daemonCfg.AllowMainnet = false
	daemonCfg.Wallet.EsploraURL = "https://esplora.example/api"
	daemonCfg.Swap.DatabaseFileName = "~/.darepod/custom-swaps.db"
	require.NoError(t, daemonCfg.Validate())

	path, err := swapStoreDatabasePath(daemonCfg, daemonCfg.Swap)
	require.NoError(t, err)
	require.Equal(
		t, filepath.Join(home, ".darepod", "custom-swaps.db"), path,
	)
}

// TestSwapStoreDatabasePathRequiresDaemonConfig verifies the helper fails
// explicitly if a future call site reaches it without daemon config.
func TestSwapStoreDatabasePathRequiresDaemonConfig(t *testing.T) {
	t.Parallel()

	path, err := swapStoreDatabasePath(nil, &darepod.SwapConfig{})
	require.ErrorContains(t, err, "daemon config is required")
	require.Empty(t, path)
}

func TestStartPayReturnsSummaryAndStartsWorker(t *testing.T) {
	t.Parallel()

	payHash := testHash(3)
	fakeClient := newFakeSwapRuntime(
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionPay,
			PaymentHash: payHash,
			State:       "created",
			Pending:     true,
			AmountSat:   10_000,
			MaxFeeSat:   25,
		},
	)
	fakeClient.startPaySession = &fakePaySession{hash: payHash}
	service := newTestSwapClientService(fakeClient)
	defer service.cancel()

	resp, err := service.StartPay(
		t.Context(), &swapclientrpc.StartPayRequest{
			Invoice:   "lnbc1test",
			MaxFeeSat: 25,
		},
	)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(payHash[:]), resp.GetPaymentHash())
	require.Equal(t, int64(10_000), resp.GetSwap().GetAmountSat())

	fakeClient.awaitPayResume(t, payHash)
	require.Equal(t, 1, fakeClient.startPayCount())
	require.Equal(t, 1, fakeClient.payResumeCount(payHash))
}

// TestStartPayPreservesRuntimeStatusCode verifies startup failures keep the
// lower-level gRPC code instead of flattening every error to Internal.
func TestStartPayPreservesRuntimeStatusCode(t *testing.T) {
	t.Parallel()

	fakeClient := newFakeSwapRuntime()
	startPayErr := status.Error(
		codes.AlreadyExists, "receive intent already used",
	)
	fakeClient.startPayErr = startPayErr
	service := newTestSwapClientService(fakeClient)
	defer service.cancel()

	_, err := service.StartPay(
		t.Context(), &swapclientrpc.StartPayRequest{
			Invoice: "lnbc1test",
		},
	)
	require.Error(t, err)
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.ErrorIs(t, err, startPayErr)
	require.Contains(t, status.Convert(err).Message(), "start pay swap")
	require.Contains(
		t, status.Convert(err).Message(),
		"receive intent already used",
	)
}

// TestStartPayRejectsInvoiceBelowOperatorDust verifies the wallet RPC facade
// applies the same operator dust limit as the underlying daemon OOR sender
// before it persists a pay swap. Without this synchronous preflight, a
// sub-dust BOLT-11 invoice could be admitted, then fail later in the
// background worker after the user-facing Send RPC already returned.
func TestStartPayRejectsInvoiceBelowOperatorDust(t *testing.T) {
	t.Parallel()

	fakeClient := newFakeSwapRuntime()
	service := newTestSwapClientService(fakeClient)
	service.chainParams = &chaincfg.RegressionNetParams
	service.payMinAmount = func(context.Context) (uint64, error) {
		return 1000, nil
	}
	defer service.cancel()

	_, err := service.StartPay(
		t.Context(), &swapclientrpc.StartPayRequest{
			Invoice: testSwapPayInvoice(t, 999),
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(
		t, err, "invoice amount_sat 999 is below the 1000 sat "+
			"minimum for pay swaps",
	)
	require.Equal(t, 0, fakeClient.startPayCount())
}

// TestStartPayRejectsMissingChainParamsAsInternal verifies a daemon wiring
// error is not reported as a caller invoice problem. Pay invoice dust
// validation needs network parameters to parse BOLT-11 correctly; if the
// service is missing them, the RPC should fail as an internal server error
// before touching the swap runtime.
func TestStartPayRejectsMissingChainParamsAsInternal(t *testing.T) {
	t.Parallel()

	fakeClient := newFakeSwapRuntime()
	service := newTestSwapClientService(fakeClient)
	service.payMinAmount = func(context.Context) (uint64, error) {
		return 1000, nil
	}
	defer service.cancel()

	_, err := service.StartPay(
		t.Context(), &swapclientrpc.StartPayRequest{
			Invoice: testSwapPayInvoice(t, 999),
		},
	)
	require.Equal(t, codes.Internal, status.Code(err))
	require.ErrorContains(
		t, err, "chain params required for pay invoice validation",
	)
	require.Equal(t, 0, fakeClient.startPayCount())
}

// TestStartReceiveReturnsInvoiceAndStartsWorker verifies receive startup
// returns invoice metadata, forwards the memo, and starts the resume worker.
func TestStartReceiveReturnsInvoiceAndStartsWorker(t *testing.T) {
	t.Parallel()

	receiveHash := testHash(4)
	fakeClient := newFakeSwapRuntime(
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionReceive,
			PaymentHash: receiveHash,
			State:       "invoice_created",
			Pending:     true,
			AmountSat:   50_000,
		},
	)
	fakeClient.startReceiveSession = &fakeReceiveSession{
		hash:    receiveHash,
		invoice: "lnbc1receive",
	}
	service := newTestSwapClientService(fakeClient)
	defer service.cancel()

	resp, err := service.StartReceive(
		t.Context(), &swapclientrpc.StartReceiveRequest{
			AmountSat: 50_000,
			Memo:      "coffee",
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, hex.EncodeToString(receiveHash[:]), resp.GetPaymentHash(),
	)
	require.Equal(t, "lnbc1receive", resp.GetInvoice())

	fakeClient.awaitReceiveResume(t, receiveHash)
	require.Equal(t, 1, fakeClient.startReceiveCount())
	require.Equal(t, "coffee", fakeClient.startReceiveMemo)
	require.Equal(t, 1, fakeClient.receiveResumeCount(receiveHash))
}

// TestStartReceiveRejectsAmountBelowOperatorDust proves receive startup fails
// before sdk/swaps creates a session or invoice when the requested amount is
// below the operator dust limit cached in darepod's server terms.
func TestStartReceiveRejectsAmountBelowOperatorDust(t *testing.T) {
	t.Parallel()

	fakeClient := newFakeSwapRuntime()
	service := newTestSwapClientService(fakeClient)
	service.receiveMinAmount = func(context.Context) (uint64, error) {
		return 1_000, nil
	}
	defer service.cancel()

	_, err := service.StartReceive(
		t.Context(), &swapclientrpc.StartReceiveRequest{
			AmountSat: 300,
		},
	)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "300")
	require.Contains(t, status.Convert(err).Message(), "1000 sat minimum")
	require.Contains(
		t, status.Convert(err).Message(),
		"operator dust limit",
	)
	require.Equal(t, 0, fakeClient.startReceiveCount())
}

func TestResumeSwapValidatesPaymentHashAndDirection(t *testing.T) {
	t.Parallel()

	hash := testHash(5)
	service := newTestSwapClientService(
		newFakeSwapRuntime(
			swaps.SwapSummary{
				Direction:   swaps.SwapDirectionPay,
				PaymentHash: hash,
				State:       "created",
				Pending:     true,
			},
		),
	)
	defer service.cancel()

	_, err := service.ResumeSwap(
		t.Context(), &swapclientrpc.ResumeSwapRequest{
			PaymentHash: "not-hex",
			Direction: swapclientrpc.
				SwapDirection_SWAP_DIRECTION_PAY,
		},
	)
	require.Error(t, err)

	_, err = service.ResumeSwap(
		t.Context(), &swapclientrpc.ResumeSwapRequest{
			PaymentHash: hex.EncodeToString(hash[:]),
		},
	)
	require.Error(t, err)
}

func TestStartRejectsReservedIdempotencyKey(t *testing.T) {
	t.Parallel()

	service := newTestSwapClientService(newFakeSwapRuntime())
	defer service.cancel()

	_, err := service.StartPay(
		t.Context(), &swapclientrpc.StartPayRequest{
			Invoice:        "lnbc1test",
			IdempotencyKey: "future-key",
		},
	)
	require.Error(t, err)

	_, err = service.StartReceive(
		t.Context(), &swapclientrpc.StartReceiveRequest{
			AmountSat:      50_000,
			IdempotencyKey: "future-key",
		},
	)
	require.Error(t, err)
}

// TestSwapSummaryToProtoCopiesDurableFields verifies every durable swap summary
// field copied into the RPC response.
func TestSwapSummaryToProtoCopiesDurableFields(t *testing.T) {
	t.Parallel()

	hash := testHash(6)
	createdAt := time.Unix(100, 0)
	updatedAt := time.Unix(200, 0)
	deadline := time.Unix(300, 0)
	senderSeed := testHash(9)
	_, senderPubKey := btcec.PrivKeyFromBytes(senderSeed[:])
	senderPubKeyHex := hex.EncodeToString(
		senderPubKey.SerializeCompressed(),
	)

	got := swapSummaryToProto(swaps.SwapSummary{
		Direction:        swaps.SwapDirectionReceive,
		PaymentHash:      hash,
		Invoice:          "lnbc1summary",
		State:            "Completed",
		Pending:          false,
		AmountSat:        1_000,
		FeeSat:           20,
		MaxFeeSat:        30,
		VHTLCOutpoint:    "txid:0",
		VHTLCAmountSat:   990,
		FundingSessionID: "funding",
		ClaimSessionID:   "claim",
		RefundSessionID:  "refund",
		TerminalReason:   "done",
		CreatedAt:        createdAt,
		UpdatedAt:        updatedAt,
		Deadline:         deadline,
		RefundLocktime:   42,
		SettlementType:   swaps.SettlementTypeInArk,
		SenderPubkey:     senderPubKey,
	})

	require.Equal(
		t, swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE,
		got.GetDirection(),
	)
	require.Equal(t, hex.EncodeToString(hash[:]), got.GetPaymentHash())
	require.Equal(t, "lnbc1summary", got.GetInvoice())
	require.Equal(
		t, swapclientrpc.SwapState_SWAP_STATE_COMPLETED, got.GetState(),
	)
	require.False(t, got.GetPending())
	require.Equal(t, int64(1_000), got.GetAmountSat())
	require.Equal(t, uint64(20), got.GetFeeSat())
	require.Equal(t, uint64(30), got.GetMaxFeeSat())
	require.Equal(t, "txid:0", got.GetVhtlcOutpoint())
	require.Equal(t, int64(990), got.GetVhtlcAmountSat())
	require.Equal(t, "funding", got.GetFundingSessionId())
	require.Equal(t, "claim", got.GetClaimSessionId())
	require.Equal(t, "refund", got.GetRefundSessionId())
	require.Equal(t, "done", got.GetTerminalReason())
	require.Equal(t, createdAt.Unix(), got.GetCreatedAtUnix())
	require.Equal(t, updatedAt.Unix(), got.GetUpdatedAtUnix())
	require.Equal(t, deadline.Unix(), got.GetDeadlineUnix())
	require.Equal(t, uint32(42), got.GetRefundLocktime())
	require.Equal(
		t, swapclientrpc.SwapSettlementType_SWAP_SETTLEMENT_TYPE_IN_ARK,
		got.GetSettlementType(),
	)
	require.Equal(t, senderPubKeyHex, got.GetSenderPubkey())
}

// TestNewSwapClientServiceRequiresRecoveryPreimageRegistry verifies that the
// swap runtime service does not start when the daemon-side vHTLC recovery
// preimage registry is unavailable. Claim recovery depends on this registration
// to look up swap-owned preimages after restart, so accepting a nil daemon
// backend would make recovery appear armed while it could not actually claim.
func TestNewSwapClientServiceRequiresRecoveryPreimageRegistry(t *testing.T) {
	t.Parallel()

	rpcServer := darepod.NewRPCServer(nil)
	daemonCfg := &darepod.Config{
		DataDir: t.TempDir(),
		Network: "regtest",
		Swap: &darepod.SwapConfig{
			ServerAddress:  "localhost:10030",
			ServerInsecure: true,
		},
	}

	service, cleanup, err := newSwapClientService(
		t.Context(), rpcServer, daemonCfg,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "register recovery preimage resolver")
	require.Nil(t, service)
	require.Nil(t, cleanup)
}

func TestNewSwapServerClientsREST(t *testing.T) {
	t.Parallel()

	nodePriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nodeID := nodePriv.PubKey().SerializeCompressed()
	routeHint := &swaprpc.RouteHint{
		NodeId:             nodeID,
		ChannelId:          42,
		FeeBaseMsat:        1,
		FeeProportionalPpm: 2,
		CltvExpiryDelta:    40,
	}
	channelIDResp := &swaprpc.RequestChannelIdResponse{
		RouteHint: routeHint,
	}

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(
					"Content-Type", "application/json",
				)

				var (
					msg        []byte
					marshalErr error
				)
				switch r.URL.Path {
				case "/v1/swap/request-channel-id":
					msg, marshalErr = protojson.Marshal(
						channelIDResp,
					)

				case "/v1/mailbox/pull":
					requireMailboxAuth(t, r)

					msg, marshalErr = protojson.Marshal(
						&mailboxpb.PullResponse{},
					)

				case "/v1/mailbox/send":
					requireMailboxAuth(t, r)

					msg, marshalErr = protojson.Marshal(
						&mailboxpb.SendResponse{},
					)

				case "/v1/mailbox/ack-up-to":
					requireMailboxAuth(t, r)

					msg, marshalErr = protojson.Marshal(
						&mailboxpb.AckUpToResponse{},
					)

				default:
					http.NotFound(w, r)

					return
				}
				require.NoError(t, marshalErr)

				_, err := w.Write(msg)
				require.NoError(t, err)
			},
		),
	)
	defer server.Close()

	clients, err := newSwapServerClients(&darepod.SwapConfig{
		ServerTransport: darepod.RPCTransportREST,
		ServerInsecure:  true,
	}, server.URL, func(_ context.Context, recipient string) (string,
		error) {

		return "auth-" + recipient, nil
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, clients.server)
	require.NotNil(t, clients.mailbox)
	require.NoError(t, clients.cleanup())

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	hint, err := clients.server.RequestChannelID(
		t.Context(), clientPriv.PubKey(), lntypes.Hash{1},
		btcutil.Amount(42_000), 30,
	)
	require.NoError(t, err)
	require.Equal(t, uint64(42), hint.RouteHint.ChannelID)
	require.Equal(t, nodeID, hint.RouteHint.NodeID)

	_, err = clients.mailbox.Pull(
		t.Context(), &mailboxpb.PullRequest{
			MailboxId: "mailbox",
		},
	)
	require.NoError(t, err)

	_, err = clients.mailbox.Send(
		t.Context(), &mailboxpb.SendRequest{
			Envelope: &mailboxpb.Envelope{
				Recipient: "mailbox",
			},
		},
	)
	require.NoError(t, err)

	_, err = clients.mailbox.AckUpTo(
		t.Context(), &mailboxpb.AckUpToRequest{
			MailboxId: "mailbox",
			Cursor:    1,
		},
	)
	require.NoError(t, err)
}

func TestNewSwapServerClientsUnknownTransport(t *testing.T) {
	t.Parallel()

	_, err := newSwapServerClients(&darepod.SwapConfig{
		ServerTransport: "webdav",
	}, "localhost:10030", nil, nil)
	require.ErrorContains(t, err, "unknown swap server transport")
}

func TestDefaultLocalSwapServerUsesInsecureTransport(t *testing.T) {
	t.Parallel()

	cfg := &darepod.SwapConfig{}

	require.True(
		t, useInsecureSwapServerTransport(
			cfg, "localhost:10030",
		),
	)
	require.Equal(
		t, "http://localhost:10030",
		swapServerRESTBaseURL(cfg, "localhost:10030"),
	)
}

func TestRemoteSwapServerUsesTLSByDefault(t *testing.T) {
	t.Parallel()

	cfg := &darepod.SwapConfig{}

	require.False(
		t, useInsecureSwapServerTransport(
			cfg, "swap.example.com:10030",
		),
	)
	require.Equal(
		t, "https://swap.example.com:10030",
		swapServerRESTBaseURL(cfg, "swap.example.com:10030"),
	)
}

func TestSwapServerTLSCertPathOverridesLocalFallback(t *testing.T) {
	t.Parallel()

	cfg := &darepod.SwapConfig{
		ServerTLSCertPath: "/tmp/swapd.pem",
	}

	require.False(
		t, useInsecureSwapServerTransport(
			cfg, "localhost:10030",
		),
	)
	require.Equal(
		t, "https://localhost:10030",
		swapServerRESTBaseURL(cfg, "localhost:10030"),
	)
}

func requireMailboxAuth(t *testing.T, r *http.Request) {
	t.Helper()

	require.Equal(
		t, "auth-mailbox", r.Header.Get(serverconn.AuthHeaderKey),
	)
}

func newTestSwapClientService(client swapRuntimeClient) *swapClientService {
	ctx, cancel := context.WithCancel(context.Background())

	return &swapClientService{
		client:      client,
		log:         btclog.Disabled,
		rootCtx:     ctx,
		cancel:      cancel,
		active:      make(map[string]struct{}),
		subscribers: make(map[chan *swapclientrpc.SwapSummary]struct{}),
	}
}

func testHash(seed byte) lntypes.Hash {
	var hash lntypes.Hash
	for i := range hash {
		hash[i] = seed
	}

	return hash
}

func testSwapPayInvoice(t *testing.T, amountSat btcutil.Amount) string {
	t.Helper()

	invoiceKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage := lntypes.Preimage{0x60, 0x6, 0x60, 0x6}
	invoice, err := zpay32.NewInvoice(
		&chaincfg.RegressionNetParams, preimage.Hash(), time.Now(),
		zpay32.Amount(
			lnwire.NewMSatFromSatoshis(amountSat),
		),
		zpay32.Description("pay"),
	)
	require.NoError(t, err)

	paymentRequest, err := invoice.Encode(zpay32.MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			return ecdsa.SignCompact(invoiceKey, msg, true), nil
		},
	})
	require.NoError(t, err)

	return paymentRequest
}

type fakeSwapRuntime struct {
	mu sync.Mutex

	summaries []swaps.SwapSummary

	startPaySession     paySwapSession
	startReceiveSession receiveSwapSession
	startPayErr         error

	startPayCalls      int
	startReceiveCalls  int
	startReceiveMemo   string
	getSummaryCalls    int
	listPendingOnly    []bool
	payResumeCalls     map[lntypes.Hash]int
	receiveResumeCalls map[lntypes.Hash]int

	payResumeCh     chan lntypes.Hash
	receiveResumeCh chan lntypes.Hash
}

func newFakeSwapRuntime(summaries ...swaps.SwapSummary) *fakeSwapRuntime {
	return &fakeSwapRuntime{
		summaries:          summaries,
		payResumeCalls:     make(map[lntypes.Hash]int),
		receiveResumeCalls: make(map[lntypes.Hash]int),
		payResumeCh:        make(chan lntypes.Hash, 8),
		receiveResumeCh:    make(chan lntypes.Hash, 8),
	}
}

func (f *fakeSwapRuntime) StartPayViaLightning(context.Context, string,
	uint64) (paySwapSession, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.startPayCalls++
	if f.startPayErr != nil {
		return nil, f.startPayErr
	}
	if f.startPaySession == nil {
		return nil, errors.New("start pay session not configured")
	}

	return f.startPaySession, nil
}

func (f *fakeSwapRuntime) StartReceiveViaLightning(_ context.Context,
	_ btcutil.Amount, memo string) (receiveSwapSession, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.startReceiveCalls++
	f.startReceiveMemo = memo
	if f.startReceiveSession == nil {
		return nil, errors.New("start receive session not configured")
	}

	return f.startReceiveSession, nil
}

func (f *fakeSwapRuntime) ResumePayViaLightning(_ context.Context,
	hash lntypes.Hash) (paySwapSession, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.payResumeCalls[hash]++
	f.payResumeCh <- hash

	return &fakePaySession{hash: hash}, nil
}

func (f *fakeSwapRuntime) ResumeReceiveViaLightning(_ context.Context,
	hash lntypes.Hash) (receiveSwapSession, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.receiveResumeCalls[hash]++
	f.receiveResumeCh <- hash

	return &fakeReceiveSession{hash: hash}, nil
}

func (f *fakeSwapRuntime) GetSwapSummary(_ context.Context, hash lntypes.Hash) (
	swaps.SwapSummary, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.getSummaryCalls++
	for _, summary := range f.summaries {
		if summary.PaymentHash == hash {
			return summary, nil
		}
	}

	return swaps.SwapSummary{}, swaps.ErrSwapSummaryNotFound
}

func (f *fakeSwapRuntime) ListSwapSummaries(_ context.Context,
	pendingOnly bool) ([]swaps.SwapSummary, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.listPendingOnly = append(f.listPendingOnly, pendingOnly)

	summaries := make([]swaps.SwapSummary, 0, len(f.summaries))
	for _, summary := range f.summaries {
		if pendingOnly && !summary.Pending {
			continue
		}

		summaries = append(summaries, summary)
	}

	return summaries, nil
}

func (f *fakeSwapRuntime) awaitPayResume(t *testing.T, hash lntypes.Hash) {
	t.Helper()

	select {
	case got := <-f.payResumeCh:
		require.Equal(t, hash, got)

	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for pay resume")
	}
}

func (f *fakeSwapRuntime) awaitReceiveResume(t *testing.T, hash lntypes.Hash) {
	t.Helper()

	select {
	case got := <-f.receiveResumeCh:
		require.Equal(t, hash, got)

	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for receive resume")
	}
}

func (f *fakeSwapRuntime) startPayCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.startPayCalls
}

func (f *fakeSwapRuntime) startReceiveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.startReceiveCalls
}

func (f *fakeSwapRuntime) payResumeCount(hash lntypes.Hash) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.payResumeCalls[hash]
}

func (f *fakeSwapRuntime) receiveResumeCount(hash lntypes.Hash) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.receiveResumeCalls[hash]
}

func (f *fakeSwapRuntime) sawPendingOnlyList() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, pendingOnly := range f.listPendingOnly {
		if pendingOnly {
			return true
		}
	}

	return false
}

type fakePaySession struct {
	hash lntypes.Hash
}

func (f *fakePaySession) PaymentHash() lntypes.Hash {
	return f.hash
}

func (f *fakePaySession) Wait(ctx context.Context) (*swaps.PayResult, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

type fakeReceiveSession struct {
	hash    lntypes.Hash
	invoice string
}

func (f *fakeReceiveSession) PaymentHash() lntypes.Hash {
	return f.hash
}

func (f *fakeReceiveSession) Invoice() string {
	return f.invoice
}

func (f *fakeReceiveSession) Wait(ctx context.Context) (*swaps.ReceiveResult,
	error) {

	<-ctx.Done()

	return nil, ctx.Err()
}
