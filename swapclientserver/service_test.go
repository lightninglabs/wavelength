//go:build swapruntime

package swapclientserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/sdk/swaps"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
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

// TestChainParamsForNetworkAcceptsTestNet4 verifies the swapruntime daemon
// subserver accepts every network string the main daemon config advertises.
func TestChainParamsForNetworkAcceptsTestNet4(t *testing.T) {
	t.Parallel()

	params, err := chainParamsForNetwork("testnet4")
	require.NoError(t, err)
	require.Same(t, &chaincfg.TestNet4Params, params)
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
	path, err := swapStoreDatabasePath(&waved.Config{
		DataDir: dataDir,
		Network: "signet",
	}, &waved.SwapConfig{})
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

	daemonCfg := waved.DefaultConfig()
	daemonCfg.Network = "regtest"
	daemonCfg.Wallet.EsploraURL = "https://esplora.example/api"
	require.NoError(t, daemonCfg.Validate())

	path, err := swapStoreDatabasePath(daemonCfg, &waved.SwapConfig{})
	require.NoError(t, err)
	require.Equal(
		t, filepath.Join(
			home, ".waved", "data", "regtest", "swaps.db",
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

	daemonCfg := waved.DefaultConfig()
	daemonCfg.DataDir = t.TempDir()
	daemonCfg.Network = "signet"
	daemonCfg.AllowMainnet = false
	daemonCfg.Wallet.EsploraURL = "https://esplora.example/api"
	daemonCfg.Swap.DatabaseFileName = "~/.waved/custom-swaps.db"
	require.NoError(t, daemonCfg.Validate())

	path, err := swapStoreDatabasePath(daemonCfg, daemonCfg.Swap)
	require.NoError(t, err)
	require.Equal(
		t, filepath.Join(home, ".waved", "custom-swaps.db"), path,
	)
}

// TestSwapStoreDatabasePathRequiresDaemonConfig verifies the helper fails
// explicitly if a future call site reaches it without daemon config.
func TestSwapStoreDatabasePathRequiresDaemonConfig(t *testing.T) {
	t.Parallel()

	path, err := swapStoreDatabasePath(nil, &waved.SwapConfig{})
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

// TestQuotePayReturnsRemotePreview verifies the local swap client RPC exposes
// the SDK's non-mutating pay quote without starting a background worker.
func TestQuotePayReturnsRemotePreview(t *testing.T) {
	t.Parallel()

	payHash := testHash(23)
	expiresAt := time.Unix(1_700_200_000, 0)
	fakeClient := newFakeSwapRuntime()
	fakeClient.quotePayResp = &swaps.InSwapQuote{
		PaymentHash:      payHash,
		InvoiceAmountSat: 10_000,
		AmountSat:        10_210,
		FeeSat:           210,
		Expiry:           expiresAt,
		SettlementType:   swaps.SettlementTypeLightning,
		ExceedsMaxFee:    true,
	}
	service := newTestSwapClientService(fakeClient)
	defer service.cancel()

	resp, err := service.QuotePay(
		t.Context(), &swapclientrpc.QuotePayRequest{
			Invoice:   "lnbc1test",
			MaxFeeSat: 1,
		},
	)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(payHash[:]), resp.GetPaymentHash())
	require.Equal(t, uint64(10_000), resp.GetInvoiceAmountSat())
	require.Equal(t, uint64(10_210), resp.GetAmountSat())
	require.Equal(t, uint64(210), resp.GetFeeSat())
	require.Equal(
		t, swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_LIGHTNING,
		resp.GetSettlementType(),
	)
	require.Equal(t, expiresAt.Unix(), resp.GetExpiresAtUnix())
	require.True(t, resp.GetExceedsMaxFee())
	require.Equal(t, 1, fakeClient.quotePayCalls)
	require.Equal(t, "lnbc1test", fakeClient.quotePayInvoice)
	require.Equal(t, uint64(1), fakeClient.quotePayMaxFeeSat)
	require.Equal(t, 0, fakeClient.startPayCount())
}

// TestStartPayReturnsPendingDuplicateInvoice verifies a repeated StartPay for
// an invoice with a pending local swap returns that durable swap summary before
// it can create another server-side in-swap.
func TestStartPayReturnsPendingDuplicateInvoice(t *testing.T) {
	t.Parallel()

	payHash := testHash(33)
	invoice := testStartPayInvoice(t, payHash, 10_000)
	fakeClient := newFakeSwapRuntime(
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionPay,
			PaymentHash: payHash,
			Invoice:     invoice,
			State:       "FundingInitiated",
			Pending:     true,
			AmountSat:   10_000,
			MaxFeeSat:   25,
		},
	)
	service := newTestSwapClientService(fakeClient)
	service.chainParams = &chaincfg.RegressionNetParams
	defer service.cancel()

	resp, err := service.StartPay(
		t.Context(), &swapclientrpc.StartPayRequest{
			Invoice:   invoice,
			MaxFeeSat: 25,
		},
	)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(payHash[:]), resp.GetPaymentHash())
	require.True(t, resp.GetSwap().GetPending())
	require.Equal(
		t, swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		resp.GetSwap().GetDirection(),
	)
	require.Equal(
		t, swapclientrpc.SwapState_SWAP_STATE_FUNDING_INITIATED,
		resp.GetSwap().GetState(),
	)

	fakeClient.awaitPayResume(t, payHash)
	require.Equal(t, 0, fakeClient.startPayCount())
	require.Equal(t, 1, fakeClient.payResumeCount(payHash))
}

// TestStartPayIgnoresPendingReceiveDuplicate verifies a receive swap with the
// invoice payment hash does not satisfy the duplicate-pay preflight.
func TestStartPayIgnoresPendingReceiveDuplicate(t *testing.T) {
	t.Parallel()

	receiveHash := testHash(34)
	payHash := testHash(35)
	invoice := testStartPayInvoice(t, receiveHash, 10_000)
	fakeClient := newFakeSwapRuntime(
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionReceive,
			PaymentHash: receiveHash,
			Invoice:     invoice,
			State:       "VHTLCAccepted",
			Pending:     true,
			AmountSat:   10_000,
		},
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionPay,
			PaymentHash: payHash,
			Invoice:     invoice,
			State:       "SwapCreated",
			Pending:     true,
			AmountSat:   10_000,
		},
	)
	fakeClient.startPaySession = &fakePaySession{hash: payHash}
	service := newTestSwapClientService(fakeClient)
	service.chainParams = &chaincfg.RegressionNetParams
	defer service.cancel()

	resp, err := service.StartPay(
		t.Context(), &swapclientrpc.StartPayRequest{
			Invoice:   invoice,
			MaxFeeSat: 25,
		},
	)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(payHash[:]), resp.GetPaymentHash())
	require.Equal(
		t, swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		resp.GetSwap().GetDirection(),
	)

	fakeClient.awaitPayResume(t, payHash)
	require.Equal(t, 1, fakeClient.startPayCount())
	require.Equal(t, 1, fakeClient.payResumeCount(payHash))
	require.Equal(t, 0, fakeClient.receiveResumeCount(receiveHash))
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

// TestStartPayAllowsInvoiceBelowOperatorDust verifies sub-dust pays are no
// longer rejected by the daemon facade before the swap server can quote a
// credit-backed payment.
func TestStartPayAllowsInvoiceBelowOperatorDust(t *testing.T) {
	t.Parallel()

	fakeClient := newFakeSwapRuntime()
	service := newTestSwapClientService(fakeClient)
	service.chainParams = &chaincfg.RegressionNetParams
	defer service.cancel()

	_, err := service.StartPay(
		t.Context(), &swapclientrpc.StartPayRequest{
			Invoice: testSwapPayInvoice(t, 999),
		},
	)
	require.ErrorContains(t, err, "start pay session not configured")
	require.Equal(t, 1, fakeClient.startPayCount())
}

// TestStartPayDefersMissingChainParamsToRuntime verifies pay invoice amount
// validation no longer requires daemon-local chain params before the swap
// server can quote credit-backed pays.
func TestStartPayDefersMissingChainParamsToRuntime(t *testing.T) {
	t.Parallel()

	fakeClient := newFakeSwapRuntime()
	service := newTestSwapClientService(fakeClient)
	defer service.cancel()

	_, err := service.StartPay(
		t.Context(), &swapclientrpc.StartPayRequest{
			Invoice: testSwapPayInvoice(t, 999),
		},
	)
	require.ErrorContains(t, err, "start pay session not configured")
	require.Equal(t, 1, fakeClient.startPayCount())
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

// TestStartReceiveReturnsCreditAssistedPlan verifies receive startup returns
// the server's credit-assisted plan even before a vHTLC outpoint exists
// locally.
func TestStartReceiveReturnsCreditAssistedPlan(t *testing.T) {
	t.Parallel()

	receiveHash := testHash(49)
	fakeClient := newFakeSwapRuntime(
		swaps.SwapSummary{
			Direction:          swaps.SwapDirectionReceive,
			PaymentHash:        receiveHash,
			State:              "invoice_created",
			Pending:            true,
			AmountSat:          300,
			RequestedAmountSat: 300,
			AvailableCreditSat: 800,
			AttachedCreditSat:  800,
			DustLimitSat:       1_000,
			SettlementType:     swaps.SettlementTypeMixed,
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
			AmountSat: 300,
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, fakeClient.startReceiveCount())
	require.Equal(t, uint64(300), resp.GetRequestedAmountSat())
	require.Equal(t, uint64(800), resp.GetAvailableCreditSat())
	require.Equal(t, uint64(800), resp.GetAttachedCreditSat())
	require.Equal(t, uint64(1_100), resp.GetVhtlcAmountSat())
	require.Equal(t, uint64(1_000), resp.GetDustLimitSat())
	require.Equal(
		t, swapclientrpc.SwapSettlementType_SWAP_SETTLEMENT_TYPE_MIXED,
		resp.GetSettlementType(),
	)
}

func TestCreateCreditForwardsLightningReceive(t *testing.T) {
	t.Parallel()

	hash := testHash(50)
	expiresAt := time.Now().Add(time.Hour)
	fakeClient := newFakeSwapRuntime()
	fakeClient.createCreditResp = &swaps.CreditOperation{
		OperationID: "cr_receive",
		State:       swaps.CreditStateAwaitingPayment,
		AmountSat:   123,
		PaymentHash: &hash,
		Invoice:     "lnbc1credit",
		ExpiresAt:   &expiresAt,
	}
	service := newTestSwapClientService(fakeClient)
	defer service.cancel()

	resp, err := service.CreateCredit(
		t.Context(), &swapclientrpc.CreateCreditRequest{
			IdempotencyKey: "idem-recv",
			Source: swapclientrpc.
				CreditFundingSource_CREDIT_FUNDING_SOURCE_LIGHTNING_RECEIVE,
			AmountSat: 123,
			Memo:      "dust receive",
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, fakeClient.createCreditCalls)
	require.Equal(
		t, swaps.CreditFundingLightningReceive,
		fakeClient.createCreditReq.Source,
	)
	require.Equal(t, "idem-recv", fakeClient.createCreditReq.IdempotencyKey)
	require.Equal(t, uint64(123), fakeClient.createCreditReq.AmountSat)
	require.Equal(t, "cr_receive", resp.GetOperationId())
	require.Equal(t, uint64(123), resp.GetAmountSat())
	require.Equal(t, hex.EncodeToString(hash[:]), resp.GetPaymentHash())
	require.Equal(t, "lnbc1credit", resp.GetInvoice())
	require.Equal(t, expiresAt.Unix(), resp.GetExpiresAtUnix())
}

func TestRedeemCreditForwardsDestination(t *testing.T) {
	t.Parallel()

	fakeClient := newFakeSwapRuntime()
	fakeClient.redeemCreditResp = &swaps.CreditRedemption{
		Operation: swaps.CreditOperation{
			OperationID: "cr_redeem",
			State:       swaps.CreditStateRedeemed,
		},
		DebitedSat:  1_000,
		RedeemedSat: 1_000,
		SessionID:   "oor-session",
	}
	service := newTestSwapClientService(fakeClient)
	defer service.cancel()

	resp, err := service.RedeemCredit(
		t.Context(), &swapclientrpc.RedeemCreditRequest{
			IdempotencyKey:    "idem-redeem",
			AmountSat:         1_000,
			DestinationPubkey: []byte{1, 2, 3},
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, fakeClient.redeemCreditCalls)
	require.Equal(
		t, "idem-redeem", fakeClient.redeemCreditReq.IdempotencyKey,
	)
	require.Equal(t, uint64(1_000), fakeClient.redeemCreditReq.AmountSat)
	require.Equal(
		t, []byte{1, 2, 3},
		fakeClient.redeemCreditReq.DestinationPubKey,
	)
	require.Equal(t, "cr_redeem", resp.GetOperationId())
	require.Equal(t, uint64(1_000), resp.GetDebitedSat())
	require.Equal(t, uint64(1_000), resp.GetRedeemedSat())
	require.Equal(t, "oor-session", resp.GetSessionId())
}

func TestListCreditsReturnsSnapshot(t *testing.T) {
	t.Parallel()

	hash := testHash(51)
	createdAt := time.Now()
	fakeClient := newFakeSwapRuntime()
	fakeClient.listCreditsResp = &swaps.CreditSnapshot{
		FinalizedSat: 10_000,
		ReservedSat:  3_000,
		AvailableSat: 7_000,
		Operations: []swaps.CreditOperation{{
			OperationID: "cr_pay",
			Type:        swaps.CreditOperationPay,
			State:       swaps.CreditStateDebited,
			AmountSat:   3_000,
			PaymentHash: &hash,
			DestinationKey: []byte{
				4,
				5,
				6,
			},
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		}},
		LedgerEntries: []swaps.CreditLedgerEntry{{
			EntryID:     "cle_1",
			OperationID: "cr_pay",
			Direction:   "debit",
			AmountSat:   3_000,
			CreatedAt:   createdAt,
		}},
	}
	service := newTestSwapClientService(fakeClient)
	defer service.cancel()

	resp, err := service.ListCredits(
		t.Context(), &swapclientrpc.ListCreditsRequest{
			Limit: 10,
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, fakeClient.listCreditsCalls)
	require.Equal(t, uint32(10), fakeClient.listCreditsLimit)
	require.Equal(t, uint64(7_000), resp.GetAvailableSat())
	require.Len(t, resp.GetOperations(), 1)
	require.Equal(t, "cr_pay", resp.GetOperations()[0].GetOperationId())
	require.Equal(
		t, hex.EncodeToString(hash[:]),
		resp.GetOperations()[0].GetPaymentHash(),
	)
	require.Len(t, resp.GetLedgerEntries(), 1)
	require.Equal(t, "cle_1", resp.GetLedgerEntries()[0].GetEntryId())
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

	rpcServer := waved.NewRPCServer(nil)
	daemonCfg := &waved.Config{
		DataDir: t.TempDir(),
		Network: "regtest",
		Swap: &waved.SwapConfig{
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

// TestDaemonWithLiveOperatorKeyUsesLiveFetcher verifies the daemon-hosted swap
// runtime bypasses the Ark SDK facade's cached operator key.
func TestDaemonWithLiveOperatorKeyUsesLiveFetcher(t *testing.T) {
	t.Parallel()

	stalePriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	stale := &staleOperatorDaemonConn{
		operatorKey: stalePriv.PubKey(),
	}
	daemon := daemonWithLiveOperatorKey(
		stale,
		func(context.Context) (*btcec.PublicKey, error) {
			return operatorPriv.PubKey(), nil
		},
	)

	key, err := daemon.OperatorPubKey(t.Context())
	require.NoError(t, err)
	require.True(t, key.IsEqual(operatorPriv.PubKey()))
	require.False(t, stale.called)
}

type staleOperatorDaemonConn struct {
	swaps.DaemonConn

	operatorKey *btcec.PublicKey
	called      bool
}

// OperatorPubKey returns the stale key that the live wrapper must bypass.
func (s *staleOperatorDaemonConn) OperatorPubKey(context.Context) (
	*btcec.PublicKey, error) {

	s.called = true

	return s.operatorKey, nil
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
		RouteHintPaths: []*swaprpc.RouteHintPath{{
			Hops: []*swaprpc.RouteHint{
				routeHint,
			},
		}},
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

	clients, err := newSwapServerClients(&waved.SwapConfig{
		ServerTransport: waved.RPCTransportREST,
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
	require.Len(t, hint.RouteHintPaths, 1)
	require.Len(t, hint.RouteHintPaths[0], 1)
	require.Equal(t, uint64(42), hint.RouteHintPaths[0][0].ChannelID)
	require.Equal(t, nodeID, hint.RouteHintPaths[0][0].NodeID)

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

	_, err := newSwapServerClients(&waved.SwapConfig{
		ServerTransport: "webdav",
	}, "localhost:10030", nil, nil)
	require.ErrorContains(t, err, "unknown swap server transport")
}

type lateSwapServiceServer struct {
	swaprpc.UnimplementedSwapServiceServer

	serverPubKey []byte
}

// CreateInSwap returns one valid response once the fake swap server is
// reachable.
func (s *lateSwapServiceServer) CreateInSwap(context.Context,
	*swaprpc.CreateInSwapRequest) (*swaprpc.CreateInSwapResponse, error) {

	paymentHash := lntypes.Hash{1, 2, 3}

	return &swaprpc.CreateInSwapResponse{
		PaymentHash:  append([]byte(nil), paymentHash[:]...),
		AmountSat:    10_000,
		FeeSat:       100,
		ServerPubkey: append([]byte(nil), s.serverPubKey...),
		VhtlcConfig: &swaprpc.VHTLCConfig{
			RefundLocktime:                       100,
			UnilateralClaimDelay:                 10,
			UnilateralRefundDelay:                20,
			UnilateralRefundWithoutReceiverDelay: 30,
			SwapserverPubkey: append(
				[]byte(nil), s.serverPubKey...,
			),
		},
		Expiry: timestamppb.New(time.Now().Add(time.Hour)),
	}, nil
}

type createInSwapResult struct {
	cfg *swaps.InSwapConfig
	err error
}

// TestSwapServerClientsWaitForLateGRPCServer verifies the daemon-owned gRPC
// swap server clients wait through a startup connection refusal.
func TestSwapServerClientsWaitForLateGRPCServer(t *testing.T) {
	t.Parallel()

	addr := reserveLoopbackAddr(t)
	clients, err := newSwapServerClients(
		&waved.SwapConfig{
			ServerTransport: waved.RPCTransportGRPC,
			ServerInsecure:  true,
		},
		addr, func(context.Context, string) (string, error) {
			return "auth", nil
		}, nil,
	)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, clients.cleanup())
	}()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	resultChan := make(chan createInSwapResult, 1)
	go func() {
		cfg, err := clients.server.CreateInSwap(
			ctx, "invoice", 1_000, clientKey.PubKey(),
		)
		resultChan <- createInSwapResult{
			cfg: cfg,
			err: err,
		}
	}()

	select {
	case result := <-resultChan:
		require.Failf(
			t, "CreateInSwap returned before swap server start",
			"err=%v", result.err,
		)

	case <-time.After(50 * time.Millisecond):
	}

	stopServer := startLateSwapServer(t, addr)
	defer stopServer()

	select {
	case result := <-resultChan:
		require.NoError(t, result.err)
		require.Equal(t, int64(10_000), result.cfg.AmountSat)

	case <-ctx.Done():
		require.FailNow(
			t, "CreateInSwap did not complete after server start",
		)
	}
}

// reserveLoopbackAddr returns an unused TCP address and closes its temporary
// listener so the test client can observe a connection refusal.
func reserveLoopbackAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	return addr
}

// startLateSwapServer starts the fake swap service on the previously refused
// address.
func startLateSwapServer(t *testing.T, addr string) func() {
	t.Helper()

	listener, err := net.Listen("tcp", addr)
	require.NoError(t, err)

	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	server := grpc.NewServer()
	swaprpc.RegisterSwapServiceServer(server, &lateSwapServiceServer{
		serverPubKey: serverKey.PubKey().SerializeCompressed(),
	})

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()

	return func() {
		server.Stop()
		require.NoError(t, <-serveErr)
	}
}

func TestDefaultLocalSwapServerUsesInsecureTransport(t *testing.T) {
	t.Parallel()

	cfg := &waved.SwapConfig{}

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

	cfg := &waved.SwapConfig{}

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

	cfg := &waved.SwapConfig{
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

// testStartPayInvoice returns a signed regtest invoice for StartPay tests that
// need the subserver's local payment-hash preflight to decode a real BOLT-11.
func testStartPayInvoice(t *testing.T, hash lntypes.Hash,
	amountSat int64) string {

	t.Helper()

	invoice, err := zpay32.NewInvoice(
		&chaincfg.RegressionNetParams, hash, time.Now(),
		zpay32.Amount(
			lnwire.MilliSatoshi(amountSat*1000),
		),
		zpay32.Description("swapclientserver test invoice"),
	)
	require.NoError(t, err)

	privKey, _ := btcec.PrivKeyFromBytes([]byte{77})
	encoded, err := invoice.Encode(zpay32.MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			digest := sha256.Sum256(msg)

			return ecdsa.SignCompact(privKey, digest[:], true), nil
		},
	})
	require.NoError(t, err)

	return encoded
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
	quotePayResp        *swaps.InSwapQuote
	quotePayErr         error
	startPayErr         error
	createCreditResp    *swaps.CreditOperation
	createCreditErr     error
	redeemCreditResp    *swaps.CreditRedemption
	redeemCreditErr     error
	listCreditsResp     *swaps.CreditSnapshot
	listCreditsErr      error

	quotePayCalls      int
	quotePayInvoice    string
	quotePayMaxFeeSat  uint64
	startPayMaxFeeSat  uint64
	startPayCalls      int
	startReceiveCalls  int
	startReceiveMemo   string
	createCreditCalls  int
	createCreditReq    swaps.CreateCreditRequest
	redeemCreditCalls  int
	redeemCreditReq    swaps.RedeemCreditRequest
	listCreditsCalls   int
	listCreditsLimit   uint32
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

func (f *fakeSwapRuntime) QuotePayViaLightning(_ context.Context,
	invoice string, maxFeeSat uint64) (*swaps.InSwapQuote, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.quotePayCalls++
	f.quotePayInvoice = invoice
	f.quotePayMaxFeeSat = maxFeeSat
	if f.quotePayErr != nil {
		return nil, f.quotePayErr
	}
	if f.quotePayResp == nil {
		return nil, errors.New("quote pay response not configured")
	}

	return f.quotePayResp, nil
}

func (f *fakeSwapRuntime) StartPayViaLightning(_ context.Context, _ string,
	maxFeeSat uint64) (paySwapSession, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.startPayCalls++
	f.startPayMaxFeeSat = maxFeeSat
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

func (f *fakeSwapRuntime) CreateCredit(_ context.Context,
	req swaps.CreateCreditRequest) (*swaps.CreditOperation, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.createCreditCalls++
	f.createCreditReq = req
	if f.createCreditErr != nil {
		return nil, f.createCreditErr
	}

	return f.createCreditResp, nil
}

func (f *fakeSwapRuntime) RedeemCredit(_ context.Context,
	req swaps.RedeemCreditRequest) (*swaps.CreditRedemption, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.redeemCreditCalls++
	f.redeemCreditReq = req
	if f.redeemCreditErr != nil {
		return nil, f.redeemCreditErr
	}

	return f.redeemCreditResp, nil
}

func (f *fakeSwapRuntime) ListCredits(_ context.Context, limit uint32) (
	*swaps.CreditSnapshot, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.listCreditsCalls++
	f.listCreditsLimit = limit
	if f.listCreditsErr != nil {
		return nil, f.listCreditsErr
	}

	return f.listCreditsResp, nil
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
