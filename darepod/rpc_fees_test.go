package darepod

import (
	"errors"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestEstimateFeeValidatesRequest covers the three pre-RPC
// rejection paths on EstimateFee: nil request, non-positive
// amount, and a nil upstream connection. Each path must return
// a gRPC status with a stable code so automated callers can
// retry or surface the right error without having to parse
// strings.
func TestEstimateFeeValidatesRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		r    *RPCServer
		req  *daemonrpc.EstimateFeeRequest
		code codes.Code
	}{
		{
			name: "nil request",
			r:    newTestRPCServer(),
			req:  nil,
			code: codes.InvalidArgument,
		},
		{
			name: "zero amount",
			r:    newTestRPCServer(),
			req: &daemonrpc.EstimateFeeRequest{
				AmountSat: 0,
			},
			code: codes.InvalidArgument,
		},
		{
			name: "negative amount",
			r:    newTestRPCServer(),
			req: &daemonrpc.EstimateFeeRequest{
				AmountSat: -1,
			},
			code: codes.InvalidArgument,
		},
		{
			name: "nil serverConn",
			r:    newTestRPCServer(),
			req: &daemonrpc.EstimateFeeRequest{
				AmountSat: 10_000,
			},
			code: codes.Unavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.r.EstimateFee(t.Context(), tc.req)
			require.Error(t, err)
			require.Equal(
				t, tc.code, status.Code(err),
				"expected %s, got %s: %v", tc.code,
				status.Code(err), err,
			)
		})
	}
}

// TestGetFeeHistoryValidatesRequest covers GetFeeHistory's
// pre-read guards: nil request, an offset that would overflow
// int32, and a missing ledger store. Each returns a gRPC
// status; the offset overflow in particular is the fix the
// other agent landed in a fixup commit and this test pins that
// contract.
func TestGetFeeHistoryValidatesRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  *daemonrpc.GetFeeHistoryRequest
		code codes.Code
	}{
		{
			name: "nil request",
			req:  nil,
			code: codes.InvalidArgument,
		},
		{
			name: "offset overflow",
			req: &daemonrpc.GetFeeHistoryRequest{
				Offset: uint32(math.MaxInt32) + 1,
			},
			code: codes.InvalidArgument,
		},
		{
			name: "nil ledger store",
			req: &daemonrpc.GetFeeHistoryRequest{
				Limit: 10,
			},
			code: codes.Unavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRPCServer()

			_, err := r.GetFeeHistory(t.Context(), tc.req)
			require.Error(t, err)
			require.Equal(
				t, tc.code, status.Code(err),
				"expected %s, got %s: %v", tc.code,
				status.Code(err), err,
			)
		})
	}
}

// TestListTransactionsValidatesRequest covers ListTransactions' pre-read
// guards so malformed pagination, date windows, and type filters fail before
// the DB layer is touched.
func TestListTransactionsValidatesRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  *daemonrpc.ListTransactionsRequest
		code codes.Code
	}{
		{
			name: "nil request",
			req:  nil,
			code: codes.InvalidArgument,
		},
		{
			name: "offset overflow",
			req: &daemonrpc.ListTransactionsRequest{
				Offset: uint32(math.MaxInt32) + 1,
			},
			code: codes.InvalidArgument,
		},
		{
			name: "negative from",
			req: &daemonrpc.ListTransactionsRequest{
				FromUnixS: -1,
			},
			code: codes.InvalidArgument,
		},
		{
			name: "negative to",
			req: &daemonrpc.ListTransactionsRequest{
				ToUnixS: -1,
			},
			code: codes.InvalidArgument,
		},
		{
			name: "inverted window",
			req: &daemonrpc.ListTransactionsRequest{
				FromUnixS: 20,
				ToUnixS:   10,
			},
			code: codes.InvalidArgument,
		},
		{
			name: "unknown type",
			req: &daemonrpc.ListTransactionsRequest{
				Type: "bogus",
			},
			code: codes.InvalidArgument,
		},
		{
			name: "nil ledger store",
			req: &daemonrpc.ListTransactionsRequest{
				Limit: 10,
			},
			code: codes.Unavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRPCServer()

			_, err := r.ListTransactions(t.Context(), tc.req)
			require.Error(t, err)
			require.Equal(
				t, tc.code, status.Code(err),
				"expected %s, got %s: %v", tc.code,
				status.Code(err), err,
			)
		})
	}
}

// TestProxyUpstreamErrorPreservesCode verifies that a gRPC
// status error from the operator keeps its code while the
// message is replaced with a generic string. This matters for
// retry logic (codes.Unavailable is retryable, codes.InvalidArgument
// is not) and for not leaking operator internals in error text.
func TestProxyUpstreamErrorPreservesCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   error
		code codes.Code
	}{
		{
			name: "grpc unavailable",
			in: status.Error(
				codes.Unavailable,
				"internal backend x failed at host foo:5432",
			),
			code: codes.Unavailable,
		},
		{
			name: "grpc invalid argument",
			in: status.Error(
				codes.InvalidArgument, "amount too large",
			),
			code: codes.InvalidArgument,
		},
		{
			name: "non-grpc network error",
			in:   errors.New("dial tcp: i/o timeout"),
			code: codes.Unavailable,
		},
		{
			name: "nil passthrough",
			in:   nil,
			code: codes.OK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := proxyUpstreamError(tc.in, "generic message")

			if tc.in == nil {
				require.Nil(t, out)

				return
			}

			require.Error(t, out)
			require.Equal(
				t, tc.code, status.Code(out),
				"status code must be preserved",
			)
			require.Equal(
				t, "generic message",
				status.Convert(out).Message(),
				"upstream message must not leak",
			)
		})
	}
}

// TestLedgerEntryToProtoOORSession verifies that a ledger row carrying
// a 32-byte session_id (OOR send leg) maps every column to the correct
// proto field, including the newly-added debit/credit account names
// and bytes-typed identifiers.
func TestLedgerEntryToProtoOORSession(t *testing.T) {
	t.Parallel()

	sessionID := make([]byte, 32)
	for i := range sessionID {
		sessionID[i] = byte(i + 1)
	}

	row := &sqlc.LedgerEntry{
		EntryID:       42,
		DebitAccount:  ledger.AccountTransfersOut,
		CreditAccount: ledger.AccountVTXOBalance,
		AmountSat:     12_345,
		RoundID:       nil,
		SessionID:     sessionID,
		EventType:     ledger.EventVTXOSent,
		Description:   "oor send leg",
		CreatedAt:     1_700_000_000,
	}

	got := ledgerEntryToProto(row)

	require.Equal(t, int64(42), got.EntryId)
	require.Equal(t, ledger.EventVTXOSent, got.EventType)
	require.Equal(t, int64(12_345), got.AmountSat)
	require.Equal(t, "oor send leg", got.Description)
	require.Equal(t, int64(1_700_000_000), got.CreatedAtUnixS)
	require.Equal(t, ledger.AccountTransfersOut, got.DebitAccount)
	require.Equal(t, ledger.AccountVTXOBalance, got.CreditAccount)
	require.Empty(t, got.RoundId)
	require.Equal(t, sessionID, got.SessionId)
}

// TestLedgerEntryToProtoRoundFee verifies that an in-round boarding
// fee row carries round_id (16 bytes) and an empty session_id, so
// downstream consumers can route on length.
func TestLedgerEntryToProtoRoundFee(t *testing.T) {
	t.Parallel()

	roundID := make([]byte, 16)
	for i := range roundID {
		roundID[i] = byte(i + 100)
	}

	row := &sqlc.LedgerEntry{
		EntryID:       7,
		DebitAccount:  ledger.AccountFeesPaid,
		CreditAccount: ledger.AccountVTXOBalance,
		AmountSat:     500,
		RoundID:       roundID,
		SessionID:     nil,
		EventType:     ledger.EventBoardingFeePaid,
		Description:   "boarding fee",
		CreatedAt:     1_700_000_100,
	}

	got := ledgerEntryToProto(row)

	require.Equal(t, ledger.AccountFeesPaid, got.DebitAccount)
	require.Equal(t, ledger.AccountVTXOBalance, got.CreditAccount)
	require.Equal(t, ledger.EventBoardingFeePaid, got.EventType)
	require.Equal(t, roundID, got.RoundId)
	require.Empty(t, got.SessionId)
}

// newFakeOperatorResponse builds a canned GetInfoResponse with a
// valid (parseable) pubkey and the given MinOperatorFee. Shared by
// the autoRefreshFeeQuoter degraded-mode tests below.
func newFakeOperatorResponse(t *testing.T,
	minOperatorFee int64) *arkrpc.GetInfoResponse {

	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return &arkrpc.GetInfoResponse{
		Pubkey:         priv.PubKey().SerializeCompressed(),
		VtxoExitDelay:  144,
		MinOperatorFee: minOperatorFee,
	}
}

// TestAutoRefreshFeeQuoterQuoteFailsFallsBackToMinFee verifies that
// when the operator is reachable for GetInfo (so MinOperatorFee is
// known) but the EstimateFee RPC fails, the quoter returns the
// legacy MinOperatorFee rather than silently collapsing to zero.
// Silent zero would fail the server's #269 validateOperatorFee
// under a non-zero fee schedule, and the VTXO would eventually
// tip over into unilateral exit.
func TestAutoRefreshFeeQuoterQuoteFailsFallsBackToMinFee(t *testing.T) {
	t.Parallel()

	const wantMinFee = btcutil.Amount(500)

	svc := &fakeArkService{
		getInfoResponse: newFakeOperatorResponse(
			t, int64(wantMinFee),
		),
		estimateFeeErr: errors.New("operator overload"),
	}

	s := &Server{
		serverConn: newBufconnClient(t, svc),
		log:        btclog.Disabled,
	}

	got := s.autoRefreshFeeQuoter()(
		t.Context(), 100_000, 200,
	)
	require.Equal(
		t, wantMinFee, got, "quoter falls back to MinOperatorFee "+
			"when EstimateFee fails, not to zero",
	)
}

// TestAutoRefreshFeeQuoterTermsFailReturnsZeroFloor verifies that
// when fetchOperatorTerms fails (operator unreachable for GetInfo),
// minFee degrades to zero. The actor still calls EstimateFee, so
// if that succeeds the quoter returns the dynamic quote; if it
// also fails, the quoter returns zero. This test covers the
// both-fail branch: terms unreachable AND EstimateFee unreachable
// returns zero, matching pre-#269 behavior (safe only on zero-fee
// schedules).
func TestAutoRefreshFeeQuoterTermsAndQuoteFailReturnsZero(t *testing.T) {
	t.Parallel()

	// Leave getInfoResponse nil so GetInfo falls through to the
	// embedded Unimplemented server (which returns an error).
	svc := &fakeArkService{
		estimateFeeErr: errors.New("operator overload"),
	}

	s := &Server{
		serverConn: newBufconnClient(t, svc),
		log:        btclog.Disabled,
	}

	got := s.autoRefreshFeeQuoter()(
		t.Context(), 100_000, 200,
	)
	require.Equal(
		t, btcutil.Amount(0), got,
		"both failures → zero, matching pre-#269 behavior",
	)
}

// TestAutoRefreshFeeQuoterReturnsQuoteWhenAboveMinFee verifies that
// when both operator calls succeed and the dynamic quote exceeds
// the floor, the quoter returns the dynamic quote. This is the
// happy path: the server's validateOperatorFee expects the full
// ComputeForfeitFee amount, not the static floor.
func TestAutoRefreshFeeQuoterReturnsQuoteWhenAboveMinFee(t *testing.T) {
	t.Parallel()

	const (
		wantMinFee = btcutil.Amount(100)
		wantQuote  = btcutil.Amount(750)
	)

	svc := &fakeArkService{
		getInfoResponse: newFakeOperatorResponse(
			t, int64(wantMinFee),
		),
		response: &arkrpc.EstimateFeeResponse{
			TotalFeeSat: int64(wantQuote),
		},
	}

	s := &Server{
		serverConn: newBufconnClient(t, svc),
		log:        btclog.Disabled,
	}

	got := s.autoRefreshFeeQuoter()(
		t.Context(), 100_000, 200,
	)
	require.Equal(t, wantQuote, got,
		"quote > floor → dynamic quote wins")
}

// TestAutoRefreshFeeQuoterReturnsMinFeeWhenQuoteBelowFloor verifies
// that when the dynamic quote is at or below MinOperatorFee, the
// quoter returns the floor. Under a low-congestion schedule the
// round FSM's pre-flight check rejects any submission strictly
// below MinOperatorFee before the round is even sent to the
// server, so over-paying the floor is the safe default.
func TestAutoRefreshFeeQuoterReturnsMinFeeWhenQuoteBelowFloor(t *testing.T) {
	t.Parallel()

	const (
		wantMinFee = btcutil.Amount(500)
		smallQuote = btcutil.Amount(50)
	)

	svc := &fakeArkService{
		getInfoResponse: newFakeOperatorResponse(
			t, int64(wantMinFee),
		),
		response: &arkrpc.EstimateFeeResponse{
			TotalFeeSat: int64(smallQuote),
		},
	}

	s := &Server{
		serverConn: newBufconnClient(t, svc),
		log:        btclog.Disabled,
	}

	got := s.autoRefreshFeeQuoter()(
		t.Context(), 100_000, 200,
	)
	require.Equal(
		t, wantMinFee, got, "quote <= floor → floor wins so the "+
			"round FSM pre-flight check does not reject the "+
			"submission",
	)
}

// TestLedgerEntryToProtoExitCost verifies that an exit-cost row
// (onchain fee leg) is mapped with the on-chain fees account on the
// debit side and no round/session linkage (unilateral exits are not
// tied to round IDs).
func TestLedgerEntryToProtoExitCost(t *testing.T) {
	t.Parallel()

	row := &sqlc.LedgerEntry{
		EntryID:       19,
		DebitAccount:  ledger.AccountOnchainFees,
		CreditAccount: ledger.AccountVTXOBalance,
		AmountSat:     2_000,
		RoundID:       nil,
		SessionID:     nil,
		EventType:     ledger.EventOnchainFeePaid,
		Description:   "unilateral exit miner fee",
		CreatedAt:     1_700_000_200,
	}

	got := ledgerEntryToProto(row)

	require.Equal(t, ledger.AccountOnchainFees, got.DebitAccount)
	require.Equal(t, ledger.EventOnchainFeePaid, got.EventType)
	require.Empty(t, got.RoundId)
	require.Empty(t, got.SessionId)
}

// TestTransactionHistoryRowToProtoLedger verifies that a ledger-backed
// transaction row maps the common history fields and leaves txid empty.
func TestTransactionHistoryRowToProtoLedger(t *testing.T) {
	t.Parallel()

	row := &sqlc.ListTransactionHistoryRow{
		Source:          "ledger",
		EntryID:         99,
		TransactionType: "oor",
		Subtype:         ledger.EventVTXOSent,
		AmountSat:       12_000,
		FeeSat:          0,
		CreatedAt:       1_700_000_300,
		Status:          "recorded",
		Description:     "oor send",
		DebitAccount:    ledger.AccountTransfersOut,
		CreditAccount:   ledger.AccountVTXOBalance,
		SessionID:       make([]byte, 32),
		OutputIndex:     7,
	}

	got, err := transactionHistoryRowToProto(row)
	require.NoError(t, err)

	require.Equal(t, "ledger", got.Source)
	require.Equal(t, "oor", got.Type)
	require.Equal(t, ledger.EventVTXOSent, got.Subtype)
	require.Equal(t, int64(12_000), got.AmountSat)
	require.Equal(t, int64(1_700_000_300), got.CreatedAtUnixS)
	require.Equal(t, "recorded", got.ConfirmationStatus)
	require.Equal(t, int64(99), got.EntryId)
	require.Empty(t, got.Txid)
	require.Equal(t, int32(7), got.OutputIndex)
	require.Equal(t, ledger.AccountTransfersOut, got.DebitAccount)
	require.Equal(t, ledger.AccountVTXOBalance, got.CreditAccount)
}

// TestTransactionHistoryRowToProtoSweep verifies sweep-backed rows expose the
// transaction id, fee, and confirmation height fields.
func TestTransactionHistoryRowToProtoSweep(t *testing.T) {
	t.Parallel()

	txidBytes := make([]byte, chainhash.HashSize)
	for i := range txidBytes {
		txidBytes[i] = byte(i + 1)
	}
	wantTxid, err := chainhash.NewHash(txidBytes)
	require.NoError(t, err)

	row := &sqlc.ListTransactionHistoryRow{
		Source:             "boarding_sweep",
		Txid:               txidBytes,
		TransactionType:    "sweep",
		Subtype:            "confirmed",
		AmountSat:          50_000,
		FeeSat:             500,
		CreatedAt:          1_700_000_400,
		Status:             "confirmed",
		Description:        "boarding timeout sweep",
		ConfirmationHeight: 144,
	}

	got, err := transactionHistoryRowToProto(row)
	require.NoError(t, err)

	require.Equal(t, "sweep", got.Type)
	require.Equal(t, wantTxid.String(), got.Txid)
	require.Equal(t, int64(500), got.FeeSat)
	require.Equal(t, int32(144), got.ConfirmationHeight)
}

// TestTransactionHistoryRowToProtoRejectsBadTxID pins corrupted sweep history
// handling: bad txid blobs fail conversion rather than returning misleading
// history.
func TestTransactionHistoryRowToProtoRejectsBadTxID(t *testing.T) {
	t.Parallel()

	_, err := transactionHistoryRowToProto(&sqlc.ListTransactionHistoryRow{
		Txid: []byte{1, 2, 3},
	})
	require.ErrorContains(t, err, "decode txid")
}
