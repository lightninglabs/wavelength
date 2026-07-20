package waved

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// refreshQuoteKey dedupes operator EstimateFee calls across the selected
// VTXOs. The operator quote is a pure function of (amount, remaining
// blocks) — VTXOs from the same round share an expiry, so a wallet-wide
// dry run typically collapses to a handful of distinct quotes instead of
// one round-trip per VTXO.
type refreshQuoteKey struct {
	amountSat       int64
	remainingBlocks uint32
}

// refreshDryRunPreview services the dry_run branch of RefreshVTXOs: it
// resolves the selected VTXOs against the SQL store, echoes the preview
// outpoint list, and attaches the advisory fee estimate. It runs before
// the wallet-ready gate (mirroring the LeaveVTXOs dry-run ordering) so
// callers can preview a refresh — including its cost — without an
// unlocked wallet; everything it needs is the SQL VTXO view, the chain
// tip, and the operator connection.
func (r *RPCServer) refreshDryRunPreview(ctx context.Context,
	explicit []wire.OutPoint, all bool) (*waverpc.RefreshVTXOsResponse,
	error) {

	// A store-less daemon (partially wired setups, tests) cannot
	// resolve amounts or lifetimes: keep the pre-existing preview
	// echo for explicit outpoints and degrade the estimate instead
	// of dropping the echo along with it. The selection=all echo is
	// empty here by construction, matching the LeaveVTXOs nil-store
	// tolerance.
	if r.server.vtxoStore == nil {
		outpointStrs := make([]string, 0, len(explicit))
		for _, op := range explicit {
			outpointStrs = append(
				outpointStrs,
				fmt.Sprintf("%s:%d", op.Hash, op.Index),
			)
		}

		resp := &waverpc.RefreshVTXOsResponse{
			QueuedOutpoints: outpointStrs,
			Status:          "preview",
		}
		if len(explicit) > 0 {
			resp.FeeEstimate = &waverpc.RefreshFeeEstimate{
				EstimateError: "VTXO store unavailable; " +
					"fee estimate not computed",
			}
		}

		return resp, nil
	}

	descs, err := r.resolveRefreshPreviewTargets(ctx, explicit, all)
	if err != nil {
		return nil, err
	}

	outpointStrs := make([]string, 0, len(descs))
	for _, desc := range descs {
		outpointStrs = append(
			outpointStrs, fmt.Sprintf("%s:%d", desc.Outpoint.Hash,
				desc.Outpoint.Index),
		)
	}

	resp := &waverpc.RefreshVTXOsResponse{
		QueuedOutpoints: outpointStrs,
		Status:          "preview",
	}

	// An empty selection (refresh --all with no live VTXOs) has
	// nothing to price: leave fee_estimate unset rather than
	// attaching a vacuous zero-fee estimate a caller could misread
	// as "free".
	if len(descs) > 0 {
		resp.FeeEstimate = r.estimateRefreshFees(ctx, descs)
	}

	return resp, nil
}

// resolveRefreshPreviewTargets maps the request selection onto full VTXO
// descriptors so the preview can price each target without the caller
// supplying amounts or lifetimes. Explicit outpoints are validated
// against the store: an unknown or non-live outpoint is a caller
// mistake and surfaces as InvalidArgument, because the real refresh can
// never execute it — a preview that echoed and priced it anyway would
// defeat dry_run's role as the validity probe behind the CLI's consent
// prompt. The caller guarantees a non-nil vtxoStore.
func (r *RPCServer) resolveRefreshPreviewTargets(ctx context.Context,
	explicit []wire.OutPoint, all bool) ([]*vtxo.Descriptor, error) {

	if all {
		liveVTXOs, err := r.server.vtxoStore.ListLiveVTXOs(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list live "+
				"VTXOs: %v", err)
		}

		// Keep only VTXOs actually in LiveState, matching the
		// real refresh path's filter: anything already on its
		// way through a round must not be double-counted in the
		// preview either.
		descs := make([]*vtxo.Descriptor, 0, len(liveVTXOs))
		for _, desc := range liveVTXOs {
			if desc.Status != vtxo.VTXOStatusLive {
				continue
			}

			descs = append(descs, desc)
		}

		return descs, nil
	}

	descs := make([]*vtxo.Descriptor, 0, len(explicit))
	for _, op := range explicit {
		desc, err := r.server.vtxoStore.GetVTXO(ctx, op)
		if err != nil {
			if errors.Is(err, vtxo.ErrVTXONotFound) {
				return nil, status.Errorf(codes.InvalidArgument,
					"unknown VTXO outpoint %s:%d", op.Hash,
					op.Index)
			}

			return nil, status.Errorf(codes.Internal, "get VTXO "+
				"%s:%d: %v", op.Hash, op.Index, err)
		}

		// GetVTXO resolves rows in any lifecycle state; the --all
		// branch filters to LiveState, and the explicit branch
		// must be as strict — a spent or in-flight VTXO would
		// preview as refreshable (and be priced) while the real
		// dispatch is guaranteed to fail on it.
		if desc.Status != vtxo.VTXOStatusLive {
			return nil, status.Errorf(codes.InvalidArgument,
				"VTXO %s:%d is not refreshable (status %v)",
				op.Hash, op.Index, desc.Status)
		}

		descs = append(descs, desc)
	}

	return descs, nil
}

// degradeRefreshEstimate marks the estimate as degraded with the given
// caller-safe message. The locally computed waiver verdict survives
// degradation: a waiver-eligible selection keeps its explicit zero
// total, while any other degraded estimate leaves the total absent so a
// missing number can never be misread as a free refresh.
func degradeRefreshEstimate(est *waverpc.RefreshFeeEstimate,
	msg string) *waverpc.RefreshFeeEstimate {

	est.EstimateError = msg
	if est.FreeRefreshEligible {
		est.EstimatedTotalFeeSat = proto.Int64(0)
	}

	return est
}

// refreshQuoteSane rejects operator quotes carrying negative,
// beyond-money-supply, or internally inconsistent values. Such a quote
// is operator nonsense; it is contained here so a broken (or hostile)
// operator can neither render negative components, poison the
// selection total, nor advertise an itemization that doesn't reconcile
// with the total the seal charges.
func refreshQuoteSane(quote *arkrpc.EstimateFeeResponse) bool {
	// Bound every component individually before summing: three
	// values each capped at MaxSatoshi cannot overflow int64, so
	// the reconciliation below is wrap-free.
	maxSat := int64(btcutil.MaxSatoshi)
	components := []int64{
		quote.LiquidityFeeSat,
		quote.OnchainShareSat,
		quote.MarginSat,
	}
	var sum int64
	for _, c := range components {
		if c < 0 || c > maxSat {
			return false
		}
		sum += c
	}

	// The operator's fee schedule defines total_fee_sat as exactly
	// liquidity + on-chain share + margin; a quote whose parts
	// don't sum to its total is as nonsensical as a negative one.
	return quote.TotalFeeSat == sum && quote.TotalFeeSat <= maxSat
}

// estimateRefreshFees builds the advisory RefreshFeeEstimate for the
// resolved targets. The estimate is strictly best-effort: any failure to
// reach the chain backend or the operator sets estimate_error and
// returns whatever facts are known locally (amounts, remaining
// lifetimes, free-window membership) instead of failing the preview —
// dry_run must keep working as a validity probe in degraded mode.
//
// The free-refresh waiver (lumos#675) is applied here rather than by the
// operator: the operator's EstimateFee prices every refresh as paid, so
// quoting a free late refresh through it alone would over-quote. The
// daemon knows the advertised window (cached operator terms), each
// VTXO's remaining lifetime, and that a RefreshVTXOs selection is
// exactly the pure one-for-one renewal shape the waiver requires, so
// eligibility is computable locally. Per-outpoint rows keep the
// ordinary paid quote (components sum to the row total) and only the
// selection-level total reflects the waiver, mirroring the operator's
// all-or-nothing seal-time rule.
func (r *RPCServer) estimateRefreshFees(ctx context.Context,
	descs []*vtxo.Descriptor) *waverpc.RefreshFeeEstimate {

	est := &waverpc.RefreshFeeEstimate{}

	// The advertised free-refresh window rides on the cached
	// operator terms; a cold cache reads as "no window", which only
	// ever over-quotes (waiver missed, never invented).
	var window uint32
	if terms := r.server.loadOperatorTerms(); terms != nil {
		window = terms.FreeRefreshWindowBlocks
	}

	height, err := r.currentBlockHeight(ctx)
	if err != nil {
		r.server.log.WarnS(ctx, "Refresh fee estimate: chain "+
			"height unavailable", err)

		// Without the chain tip neither remaining lifetimes nor
		// window membership can be computed; return the
		// amount-only rows so the caller still sees what was
		// selected.
		for _, desc := range descs {
			est.Outpoints = append(
				est.Outpoints, &waverpc.OutpointFeeEstimate{
					Outpoint: fmt.Sprintf(
						"%s:%d", desc.Outpoint.Hash,
						desc.Outpoint.Index,
					),
					AmountSat: int64(desc.Amount),
				},
			)
		}

		return degradeRefreshEstimate(
			est,
			"chain height unavailable; fee estimate not computed",
		)
	}

	// First pass: per-outpoint facts the daemon knows locally. The
	// quote's remaining-blocks figure is clamped to 1 because the
	// operator treats zero as "use the full sweep-delay lifetime",
	// which would massively over-quote an expiring VTXO; window
	// membership uses the unclamped value (an expired VTXO is not
	// waiver-eligible).
	allInWindow := true
	for _, desc := range descs {
		remaining := vtxo.BlocksUntilExpiry(desc, height)
		clamped := remaining
		if clamped < 1 {
			clamped = 1
		}

		inWindow := window > 0 && remaining > 0 &&
			uint32(remaining) <= window
		if !inWindow {
			allInWindow = false
		}

		est.Outpoints = append(
			est.Outpoints, &waverpc.OutpointFeeEstimate{
				Outpoint: fmt.Sprintf(
					"%s:%d", desc.Outpoint.Hash,
					desc.Outpoint.Index,
				),
				AmountSat:           int64(desc.Amount),
				RemainingBlocks:     uint32(clamped),
				InFreeRefreshWindow: inWindow,
			},
		)
	}
	est.FreeRefreshEligible = allInWindow

	// Second pass: fetch and validate the operator quotes, deduped
	// on (amount, remaining blocks), and pre-compute the selection
	// total. Every quote is vetted before any component is written
	// to a row, so a mid-selection failure degrades all-or-nothing —
	// rows either all carry components or none do, which is exactly
	// what estimate_error promises the caller.
	quotes := make(map[refreshQuoteKey]*arkrpc.EstimateFeeResponse)

	var total int64
	for _, row := range est.Outpoints {
		key := refreshQuoteKey{
			amountSat:       row.AmountSat,
			remainingBlocks: row.RemainingBlocks,
		}

		quote, ok := quotes[key]
		if !ok {
			var err error
			quote, err = r.server.quoteOperatorFeeBreakdown(
				ctx, key.amountSat, false, /* isBoarding */
				key.remainingBlocks,
			)
			if err != nil {
				r.server.log.WarnS(ctx, "Refresh fee "+
					"estimate: operator quote "+
					"unavailable", err)

				// Match the EstimateFee proxy's
				// sanitization stance: log the upstream
				// detail, never echo operator-internal
				// error text to the caller.
				return degradeRefreshEstimate(
					est,
					"operator fee estimate unavailable",
				)
			}

			if !refreshQuoteSane(quote) {
				r.server.log.WarnS(ctx, "Refresh fee "+
					"estimate: operator returned "+
					"nonsense fee",
					fmt.Errorf("total_fee_sat=%d for %s",
						quote.TotalFeeSat, row.Outpoint))

				return degradeRefreshEstimate(
					est, "operator fee estimate invalid",
				)
			}

			quotes[key] = quote
		}

		// Each vetted quote is bounded by MaxSatoshi, but a large
		// selection could still wrap the sum; a total beyond the
		// money supply is as nonsensical as a negative quote.
		total += quote.TotalFeeSat
		if total > int64(btcutil.MaxSatoshi) {
			r.server.log.WarnS(ctx, "Refresh fee estimate: "+
				"selection total overflows",
				fmt.Errorf("total=%d", total))

			return degradeRefreshEstimate(
				est, "operator fee estimate invalid",
			)
		}
	}

	// Third pass: apply the fully vetted quotes to the rows.
	for _, row := range est.Outpoints {
		quote := quotes[refreshQuoteKey{
			amountSat:       row.AmountSat,
			remainingBlocks: row.RemainingBlocks,
		}]

		row.LiquidityFeeSat = quote.LiquidityFeeSat
		row.OnchainShareSat = quote.OnchainShareSat
		row.MarginSat = quote.MarginSat
		row.TotalFeeSat = quote.TotalFeeSat
		row.BelowDustWarning = quote.BelowDustWarning
	}

	if est.FreeRefreshEligible {
		est.EstimatedTotalFeeSat = proto.Int64(0)
	} else {
		est.EstimatedTotalFeeSat = proto.Int64(total)
	}

	return est
}
