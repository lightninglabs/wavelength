package walletdk

import (
	"strings"
	"time"

	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
)

// swapSummaryFromProto copies the daemon RPC summary into wrapper-owned
// fields so mobile and UI callers do not need protobuf types.
func swapSummaryFromProto(summary *swapclientrpc.SwapSummary) SwapSummary {
	if summary == nil {
		return SwapSummary{}
	}

	return SwapSummary{
		Direction: swapDirectionFromProto(
			summary.GetDirection(),
		),
		PaymentHash:      summary.GetPaymentHash(),
		State:            swapStateFromProto(summary.GetState()),
		Pending:          summary.GetPending(),
		AmountSat:        summary.GetAmountSat(),
		FeeSat:           summary.GetFeeSat(),
		MaxFeeSat:        summary.GetMaxFeeSat(),
		VHTLCOutpoint:    summary.GetVhtlcOutpoint(),
		VHTLCAmountSat:   summary.GetVhtlcAmountSat(),
		FundingSessionID: summary.GetFundingSessionId(),
		ClaimSessionID:   summary.GetClaimSessionId(),
		RefundSessionID:  summary.GetRefundSessionId(),
		TerminalReason:   summary.GetTerminalReason(),
		CreatedAt:        unixTime(summary.GetCreatedAtUnix()),
		UpdatedAt:        unixTime(summary.GetUpdatedAtUnix()),
		Deadline:         unixTime(summary.GetDeadlineUnix()),
		RefundLocktime:   summary.GetRefundLocktime(),
	}
}

// swapDirectionFromProto maps the generated enum into walletdk's compact
// string-like direction type. Unknown future enum values fall back to the
// lowercased proto name (minus the SWAP_DIRECTION_ prefix) so a new value
// added to the proto surfaces in host UIs instead of erasing into "".
func swapDirectionFromProto(
	direction swapclientrpc.SwapDirection) SwapDirection {

	switch direction {
	case swapclientrpc.SwapDirection_SWAP_DIRECTION_UNSPECIFIED:
		return ""

	case swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY:
		return SwapDirectionPay

	case swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE:
		return SwapDirectionReceive

	default:
		return SwapDirection(
			strings.ToLower(
				strings.TrimPrefix(
					direction.String(),
					"SWAP_DIRECTION_",
				),
			),
		)
	}
}

// swapDirectionToProto maps the public direction back to the daemon RPC enum
// used by resume requests.
func swapDirectionToProto(direction SwapDirection) swapclientrpc.SwapDirection {
	switch direction {
	case SwapDirectionPay:
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY

	case SwapDirectionReceive:
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE

	default:
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_UNSPECIFIED
	}
}

// swapStateFromProto returns stable lowercase state names for display and
// bridge layers.
//
// The wrapper deliberately owns the string form so the SDK contract is
// independent of how the proto enum is named or renumbered. Host UIs and
// React Native / gomobile bridges can switch on these values without taking a
// dependency on the generated enum type. Unknown future enum values fall back
// to the lowercased proto name (minus the SWAP_STATE_ prefix) so a new state
// added to the proto surfaces in host UIs instead of being silently flattened
// into "unspecified".
func swapStateFromProto(state swapclientrpc.SwapState) string {
	switch state {
	case swapclientrpc.SwapState_SWAP_STATE_UNSPECIFIED:
		return "unspecified"

	case swapclientrpc.SwapState_SWAP_STATE_CREATED:
		return "created"

	case swapclientrpc.SwapState_SWAP_STATE_SWAP_CREATED:
		return "swap_created"

	case swapclientrpc.SwapState_SWAP_STATE_FUNDING_INITIATED:
		return "funding_initiated"

	case swapclientrpc.SwapState_SWAP_STATE_VHTLC_FUNDED:
		return "vhtlc_funded"

	case swapclientrpc.SwapState_SWAP_STATE_WAITING_FOR_CLAIM:
		return "waiting_for_claim"

	case swapclientrpc.SwapState_SWAP_STATE_COMPLETED:
		return "completed"

	case swapclientrpc.SwapState_SWAP_STATE_EXPIRED:
		return "expired"

	case swapclientrpc.SwapState_SWAP_STATE_REFUND_INITIATED:
		return "refund_initiated"

	case swapclientrpc.SwapState_SWAP_STATE_REFUNDED:
		return "refunded"

	case swapclientrpc.SwapState_SWAP_STATE_NEEDS_INTERVENTION:
		return "needs_intervention"

	case swapclientrpc.SwapState_SWAP_STATE_FAILED:
		return "failed"

	case swapclientrpc.SwapState_SWAP_STATE_INVOICE_CREATED:
		return "invoice_created"

	case swapclientrpc.SwapState_SWAP_STATE_CLAIM_INITIATED:
		return "claim_initiated"

	default:
		return strings.ToLower(
			strings.TrimPrefix(
				state.String(),
				"SWAP_STATE_",
			),
		)
	}
}

// unixTime preserves unset timestamp fields as zero time values.
func unixTime(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}

	return time.Unix(sec, 0)
}
