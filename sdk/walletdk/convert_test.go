package walletdk

import (
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/stretchr/testify/require"
)

// TestSwapSummaryFromProto guards the wrapper-owned accounting DTO from
// accidental protobuf field drift.
func TestSwapSummaryFromProto(t *testing.T) {
	proto := &swapclientrpc.SwapSummary{
		Direction: swapclientrpc.
			SwapDirection_SWAP_DIRECTION_PAY,
		PaymentHash:      "hash",
		State:            swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
		Pending:          true,
		AmountSat:        1000,
		FeeSat:           12,
		MaxFeeSat:        30,
		VhtlcOutpoint:    "txid:0",
		VhtlcAmountSat:   900,
		FundingSessionId: "funding",
		ClaimSessionId:   "claim",
		RefundSessionId:  "refund",
		TerminalReason:   "done",
		CreatedAtUnix:    11,
		UpdatedAtUnix:    12,
		DeadlineUnix:     13,
		RefundLocktime:   42,
	}

	summary := swapSummaryFromProto(proto)
	require.Equal(t, SwapDirectionPay, summary.Direction)
	require.Equal(t, "hash", summary.PaymentHash)
	require.Equal(t, "completed", summary.State)
	require.True(t, summary.Pending)
	require.EqualValues(t, 1000, summary.AmountSat)
	require.EqualValues(t, 12, summary.FeeSat)
	require.EqualValues(t, 30, summary.MaxFeeSat)
	require.Equal(t, "txid:0", summary.VHTLCOutpoint)
	require.EqualValues(t, 900, summary.VHTLCAmountSat)
	require.Equal(t, "funding", summary.FundingSessionID)
	require.Equal(t, "claim", summary.ClaimSessionID)
	require.Equal(t, "refund", summary.RefundSessionID)
	require.Equal(t, "done", summary.TerminalReason)
	require.Equal(t, time.Unix(11, 0), summary.CreatedAt)
	require.Equal(t, time.Unix(12, 0), summary.UpdatedAt)
	require.Equal(t, time.Unix(13, 0), summary.Deadline)
	require.EqualValues(t, 42, summary.RefundLocktime)
}

// TestSwapDirectionToProto verifies resume requests use the expected daemon
// RPC direction enum.
func TestSwapDirectionToProto(t *testing.T) {
	require.Equal(
		t, swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		swapDirectionToProto(SwapDirectionPay),
	)
	require.Equal(
		t, swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE,
		swapDirectionToProto(SwapDirectionReceive),
	)
	require.Equal(
		t, swapclientrpc.SwapDirection_SWAP_DIRECTION_UNSPECIFIED,
		swapDirectionToProto(""),
	)
}

// TestSwapStateFromProtoUnknownFallback verifies that unknown future enum
// values fall back to a lowercased proto name rather than being flattened
// into "unspecified". The latter is reserved for the explicit UNSPECIFIED
// enum value so host UIs can distinguish the two.
func TestSwapStateFromProtoUnknownFallback(t *testing.T) {
	require.Equal(
		t, "unspecified", swapStateFromProto(
			swapclientrpc.SwapState_SWAP_STATE_UNSPECIFIED,
		),
	)

	// A value that is not listed in the proto generates a String() of the
	// form "SwapState(<n>)" — the trim leaves the parenthesized form,
	// which is fine: it is a stable, non-empty signal that something new
	// is happening rather than a silent erase.
	unknown := swapStateFromProto(swapclientrpc.SwapState(9999))
	require.NotEqual(t, "unspecified", unknown)
	require.NotEmpty(t, unknown)
}

// TestSwapDirectionFromProtoUnknownFallback mirrors the state-unknown test
// for direction. Unspecified maps to the empty string; future values must
// produce a non-empty fallback.
func TestSwapDirectionFromProtoUnknownFallback(t *testing.T) {
	require.Equal(
		t, SwapDirection(""), swapDirectionFromProto(
			swapclientrpc.SwapDirection_SWAP_DIRECTION_UNSPECIFIED,
		),
	)

	unknown := swapDirectionFromProto(swapclientrpc.SwapDirection(9999))
	require.NotEmpty(t, unknown)
}
