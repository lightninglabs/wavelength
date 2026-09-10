//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// onchainTerms holds the chain height and operator policy from one GetInfo
// call. Missing operator terms leave selection headroom and the local floor
// at zero; missing height prevents a complete lifetime-aware operator quote.
type onchainTerms struct {
	blockHeight    uint32
	feeRate        btcutil.Amount
	minOperatorFee btcutil.Amount
	dustLimit      btcutil.Amount
}

// fetchOnchainTerms reads the chain height and cached operator terms once
// for coin selection and fee estimation. A failed lookup or nil response
// yields a zero-valued struct so the preview still renders.
func (r *router) fetchOnchainTerms(ctx context.Context) onchainTerms {
	info, err := r.deps.RPCServer.GetInfo(
		ctx, &waverpc.GetInfoRequest{},
	)
	if err != nil || info == nil {
		return onchainTerms{}
	}

	server := info.GetServerInfo()

	return onchainTerms{
		blockHeight:    info.GetBlockHeight(),
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
// offline. inputs contains the selected VTXOs, whose values and remaining
// lifetimes determine the operator's per-forfeit charges. sweepAll sizes
// the local fallback's outputs; terms supplies the chain height and policy.
func (r *router) estimateOnchainFee(ctx context.Context, inputs []*waverpc.VTXO,
	sweepAll bool, terms onchainTerms) (onchainFeeQuote, error) {

	fee, ok, err := r.quoteOnchainInputs(ctx, inputs, terms.blockHeight)
	if err != nil {
		return onchainFeeQuote{}, err
	}
	if ok {
		return onchainFeeQuote{
			feeSat:   fee,
			feeKnown: true,
			quoteStatus: wavewalletrpc.
				SendQuoteStatus_SEND_QUOTE_STATUS_COMPLETE,
		}, nil
	}

	// Operator quote unavailable: fall back to a local floor derived
	// from the cached operator terms. This captures the on-chain share
	// at batch size 1 plus the operator's minimum fee, but cannot see
	// the operator's liquidity/congestion components, so it is a lower
	// bound the caller must treat as LOCAL_ONLY.
	floor := localOnchainFeeFloor(len(inputs), sweepAll, terms)

	return onchainFeeQuote{
		feeSat:   floor,
		feeKnown: false,
		quoteStatus: wavewalletrpc.
			SendQuoteStatus_SEND_QUOTE_STATUS_LOCAL_ONLY,
		warning: "fee is a local estimate assuming a batch size of " +
			"one; a complete per-input operator quote was " +
			"unavailable; the binding fee is set when the round " +
			"seals",
	}, nil
}

// onchainQuoteKey identifies VTXOs that can reuse the same operator quote.
type onchainQuoteKey struct {
	amountSat       int64
	remainingBlocks uint32
}

// quoteOnchainInputs sums the operator's per-forfeit fees for a leave.
// Each input pays its own fixed components, even when all inputs belong to
// one wallet. Quoting the destination amount once misses those charges and
// also prices the wrong principal when a bounded send returns change.
// Quotes assume batch size one; the binding fee uses actual round occupancy.
// Any missing timing context or invalid quote discards the entire remote
// estimate, so a partial sum can never masquerade as a complete quote.
// An explicit economic warning instead rejects the preview: substituting a
// local floor would hide an operator quote that is already known to be costly.
func (r *router) quoteOnchainInputs(ctx context.Context, inputs []*waverpc.VTXO,
	height uint32) (int64, bool, error) {

	if height == 0 || len(inputs) == 0 {
		return 0, false, nil
	}

	quotes := make(map[onchainQuoteKey]int64)
	var total int64
	for _, input := range inputs {
		if input.GetBatchExpiry() <= 0 || input.GetAmountSat() <= 0 {
			return 0, false, nil
		}

		// Zero means "use the full default lifetime" to the operator.
		// Clamp expiring inputs to one block, as in refresh previews.
		remaining := max(int64(input.GetBatchExpiry())-int64(height), 1)
		key := onchainQuoteKey{
			amountSat:       input.GetAmountSat(),
			remainingBlocks: uint32(remaining),
		}
		fee, ok := quotes[key]
		if !ok {
			resp, err := r.deps.RPCServer.EstimateFee(
				ctx, &waverpc.EstimateFeeRequest{
					AmountSat:       key.amountSat,
					RemainingBlocks: key.remainingBlocks,
				},
			)
			if err != nil || resp == nil {

				//nolint:nilerr // Use the local floor.
				return 0, false, nil
			}
			if resp.GetBelowDustWarning() {
				const reason = "uneconomic input %s: " +
					"amount=%d sat, fee=%d sat"

				return 0, false, status.Errorf(
					codes.FailedPrecondition, reason,
					input.GetOutpoint(),
					input.GetAmountSat(),
					resp.GetTotalFeeSat())
			}

			fee = resp.GetTotalFeeSat()
			quotes[key] = fee
		}

		// Bound the addition before accumulating untrusted totals.
		if fee < 0 || fee > int64(btcutil.MaxSatoshi)-total {
			return 0, false, nil
		}
		total += fee
	}

	return total, true, nil
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
