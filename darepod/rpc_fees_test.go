package darepod

import (
	"errors"
	"testing"

	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
