//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
)

// Local cooperative-leave fee-floor sizing. These virtual sizes drive the
// offline fallback estimate only: when the operator's EstimateFee quote is
// unreachable we approximate the on-chain share the client would bear if it
// were the sole intent in the round (batch size 1). They are deliberately
// conservative so the floor never under-promises the eventual seal-time
// fee more than the (operator-only) liquidity and congestion components
// already force it to.
const (
	// leaveBaseVBytes is the fixed v3 transaction overhead attributed to
	// a cooperative-leave commitment at batch size 1: version, locktime,
	// segwit marker/flag, input/output counts, and the above-dust P2A
	// anchor every leave package carries.
	leaveBaseVBytes = 60

	// leaveInputVBytes is the virtual size of one taproot key-spend
	// forfeit input (outpoint, sequence, and a single Schnorr witness),
	// rounded up from ~57.5 vB.
	leaveInputVBytes = 58

	// leaveOutputVBytes is the virtual size of one taproot (P2TR)
	// output.
	leaveOutputVBytes = 43
)

// onchainFeeQuote is the resolved fee preview for a cooperative leave: the
// estimated fee, how complete the quote is, and an optional warning to
// surface when the number is a local floor rather than an operator quote.
type onchainFeeQuote struct {
	feeSat      int64
	feeKnown    bool
	quoteStatus wavewalletrpc.SendQuoteStatus
	warning     string
}

// onchainTerms holds the operator-policy values the preview needs from a
// single GetInfo call: the target feerate plus the minimum-operator-fee
// and dust-limit headroom the daemon selects against. All zero when GetInfo
// or its ServerInfo is unavailable, which degrades the preview to a
// zero-headroom, zero-floor estimate rather than failing outright.
type onchainTerms struct {
	feeRate        btcutil.Amount
	minOperatorFee btcutil.Amount
	dustLimit      btcutil.Amount
}

// fetchOnchainTerms reads the daemon's cached operator terms once so the
// caller can size both the coin-selection headroom and the local fee floor
// from a single lookup. A failed lookup (or a nil response/ServerInfo)
// yields a zero-valued struct so the preview still renders.
func (r *router) fetchOnchainTerms(ctx context.Context) onchainTerms {
	info, err := r.deps.RPCServer.GetInfo(
		ctx, &waverpc.GetInfoRequest{},
	)
	if err != nil || info == nil {
		return onchainTerms{}
	}

	server := info.GetServerInfo()
	if server == nil {
		return onchainTerms{}
	}

	return onchainTerms{
		feeRate:        btcutil.Amount(server.GetFeeRate()),
		minOperatorFee: btcutil.Amount(server.GetMinOperatorFee()),
		dustLimit:      btcutil.Amount(server.GetDustLimit()),
	}
}

// estimateOnchainFee resolves the prepare-time fee preview for a
// cooperative leave. It prefers the operator's dynamic EstimateFee quote
// (which folds in the on-chain share, liquidity, and margin the server
// charges at seal time) and falls back to a purely local batch-size-1
// floor when the operator is unreachable, so a preview is still produced
// offline. amountSat is the leave amount; numInputs and sweepAll size the
// local fallback's on-chain footprint; terms supplies the floor's feerate
// and minimum-fee inputs.
func (r *router) estimateOnchainFee(ctx context.Context, amountSat int64,
	numInputs int, sweepAll bool, terms onchainTerms) onchainFeeQuote {

	// Prefer the operator's dynamic quote. RemainingBlocks is zero: a
	// leave exits the funds now, so there is no residual lock time to
	// price the time-value component against. A successful quote backs
	// every fee field, so the preview is COMPLETE. The feeResp nil guard
	// is belt-and-suspenders: the generated getter is nil-safe, but the
	// explicit check keeps the intent obvious at the call site.
	feeResp, err := r.deps.RPCServer.EstimateFee(
		ctx, &waverpc.EstimateFeeRequest{
			AmountSat:       amountSat,
			IsBoarding:      false,
			RemainingBlocks: 0,
		},
	)
	if err == nil && feeResp != nil && feeResp.GetTotalFeeSat() > 0 {
		return onchainFeeQuote{
			feeSat:   feeResp.GetTotalFeeSat(),
			feeKnown: true,
			quoteStatus: wavewalletrpc.
				SendQuoteStatus_SEND_QUOTE_STATUS_COMPLETE,
		}
	}

	// Operator quote unavailable: fall back to a local floor derived
	// from the cached operator terms. This captures the on-chain share
	// at batch size 1 plus the operator's minimum fee, but cannot see
	// the operator's liquidity/congestion components, so it is a lower
	// bound the caller must treat as LOCAL_ONLY.
	floor := localOnchainFeeFloor(numInputs, sweepAll, terms)

	return onchainFeeQuote{
		feeSat:   floor,
		feeKnown: false,
		quoteStatus: wavewalletrpc.
			SendQuoteStatus_SEND_QUOTE_STATUS_LOCAL_ONLY,
		warning: "fee is a local estimate assuming a batch size of " +
			"one; the operator quote was unavailable and the " +
			"binding fee is set when the round seals",
	}
}

// localOnchainFeeFloor computes a batch-size-1 fee lower bound from the
// cached operator terms: the larger of the operator's minimum fee and the
// on-chain cost of this leave's footprint at the operator's target feerate.
// Zero-valued terms (an unavailable GetInfo) yield a zero floor rather than
// failing the preview.
func localOnchainFeeFloor(numInputs int, sweepAll bool,
	terms onchainTerms) int64 {

	// A bounded leave produces a leave output plus a change VTXO output;
	// a sweep collapses to a single fee-absorbing leave output.
	numOutputs := 2
	if sweepAll {
		numOutputs = 1
	}

	vbytes := int64(leaveBaseVBytes) +
		int64(numInputs)*leaveInputVBytes +
		int64(numOutputs)*leaveOutputVBytes

	onchainShare := int64(terms.feeRate) * vbytes

	// Floor the on-chain share at the operator's stated minimum fee:
	// the true total is at least MinOperatorFee regardless of size.
	minFee := int64(terms.minOperatorFee)
	if onchainShare > minFee {
		return onchainShare
	}

	return minFee
}
