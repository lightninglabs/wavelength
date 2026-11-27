package waved

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// WalletExitPreview contains the selected amount, per-target admission
// information and one full-recovery fee estimate for the whole wallet.
type WalletExitPreview struct {
	// Entries retains request order and reports the outcome of each
	// selected outpoint.
	Entries []WalletExitPreviewEntry

	// AmountSat sums the selected VTXO face values before fees.
	AmountSat int64

	// FeeRate is the estimated package fee rate in satoshis per virtual
	// byte.
	FeeRate int64

	// SweepAddress is the validated custom destination, if requested.
	SweepAddress string

	// Quote prices the union of the selected targets' proof transactions.
	Quote unroll.WalletExitQuote
}

// WalletExitPreviewEntry carries one target's economics and existing job.
type WalletExitPreviewEntry struct {
	// Outpoint identifies the selected VTXO by its transaction ID and
	// output index.
	Outpoint string

	// AmountSat is the face value of this VTXO before recovery fees.
	AmountSat int64

	// BatchExpiry is the absolute block height at which the batch expires.
	BatchExpiry int32

	// Status carries the daemon's current job observation, including
	// whether a job exists.
	Status *waverpc.GetUnrollStatusResponse

	// AdmissionError explains an economic refusal, such as a sweep below
	// dust.
	AdmissionError string

	// InfeasibilityReason identifies an economic refusal for display.
	InfeasibilityReason unroll.ExitInfeasibilityReason

	// RoundCommitment warns that a cooperative round holds this VTXO.
	// Manual recovery remains available.
	RoundCommitment error
}

// PreviewUnilateralExit reads local descriptors, proof graphs and confirmed
// wallet coins without deriving addresses, admitting jobs or reserving funds.
func (r *RPCServer) PreviewUnilateralExit(ctx context.Context,
	req *ExitPlanRequest) (*WalletExitPreview, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}
	if req == nil || len(req.Outpoints) == 0 || len(req.Outpoints) > 100 {
		return nil, status.Error(
			codes.InvalidArgument,
			"provide between 1 and 100 exit targets",
		)
	}
	if r.server.vtxoStore == nil {
		return nil, status.Error(
			codes.Unavailable, "VTXO store not initialized",
		)
	}
	address, options, err := r.validateManualExitTerms(req.ManualTerms)
	if err != nil {
		return nil, err
	}
	rate := req.ManualTerms.FeeRateSatPerVByte
	if rate == 0 {
		rate, err = r.estimateWalletFeeRate(ctx, req.ConfTarget)
		if err != nil {
			return nil, err
		}
	}
	resolver := unroll.DescriptorLineageResolver{
		VTXOStore: r.server.vtxoStore,
	}
	if store := r.newLocalOORArtifactStore(); store != nil {
		resolver.ArtifactStore = store
	}

	// Preserve the selection order in the RPC even though the funding
	// simulation orders targets by their batch expiry.
	result := &WalletExitPreview{
		FeeRate: rate, SweepAddress: address,
	}
	seen := make(map[wire.OutPoint]bool)
	var targets []unroll.WalletExitTarget
	for _, raw := range req.Outpoints {
		op, err := parseOutpointString(raw)
		if err != nil || seen[op] {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid or duplicate target %q", raw)
		}
		seen[op] = true

		// Allocate the entry before resolving its proof. A stale target
		// keeps its position without suppressing viable siblings.
		result.Entries = append(result.Entries, WalletExitPreviewEntry{
			Outpoint: op.String(),
		})
		entry := &result.Entries[len(result.Entries)-1]
		desc, err := r.server.vtxoStore.GetVTXO(ctx, op)
		switch {
		case errors.Is(err, vtxo.ErrVTXONotFound):
			entry.AdmissionError = err.Error()
			continue

		case err != nil:
			return nil, fmt.Errorf("load exit descriptor %s: %w",
				op, err)
		}

		entry.AmountSat = int64(desc.Amount)
		entry.BatchExpiry = desc.BatchExpiry
		result.AmountSat += entry.AmountSat

		job, err := r.GetUnrollStatus(
			ctx, &waverpc.GetUnrollStatusRequest{
				Outpoint: op.String(),
			},
		)
		if err != nil {
			return nil, err
		}
		entry.Status = job

		// Completed jobs have no remaining cost. For active jobs this
		// API estimates the full proof, including already mined work.
		if job.GetStatus() ==
			waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED {

			continue
		}
		if err := validateManualExitTarget(desc, options); err != nil {
			entry.AdmissionError = status.Convert(err).Message()

			continue
		}
		mat, err := resolver.ResolveLineage(ctx, op)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			entry.AdmissionError = fmt.Sprintf("resolve "+
				"lineage: %v", err)

			continue
		}
		proof, err := unroll.BuildProofFromMaterial(mat)
		if err != nil {
			entry.AdmissionError = fmt.Sprintf("assemble proof: %v",
				err)

			continue
		}

		entry.setFundingEligibility(
			desc, mat, btcutil.Amount(rate), options,
		)
		if entry.AdmissionError != "" {
			continue
		}
		targets = append(targets, unroll.WalletExitTarget{
			Proof:  proof,
			Expiry: desc.BatchExpiry,
			Asset:  desc.TaprootAssetRoot != nil,
		})
	}

	if len(targets) == 0 {
		return result, nil
	}

	// All targets draw on the same snapshot of spendable wallet coins.
	utxos, err := r.server.ListWalletUnspent(
		ctx, int32(unroll.RequiredFeeInputConfirmations),
		wallet.MaxConfsForListUnspent,
	)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "list wallet fee "+
			"coins: %v", err)
	}
	var amounts []btcutil.Amount
	for _, coin := range utxos {
		if coin != nil {
			amounts = append(amounts, coin.Amount)
		}
	}
	result.Quote, err = unroll.QuoteWalletExit(
		targets, amounts, btcutil.Amount(rate),
	)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// setFundingEligibility keeps a live cooperative round visible to the caller
// while preserving the manual recovery path when the operator is unavailable.
func (e *WalletExitPreviewEntry) setFundingEligibility(desc *vtxo.Descriptor,
	mat *unroll.LineageMaterial, rate btcutil.Amount,
	options *actormsg.ManualExitOptions) {

	e.RoundCommitment = exitPlanRoundCommitment(desc)
	verdict := unroll.PlanExitFundingWithOptions(
		desc, mat, rate, unroll.ExitFundingSnapshot{}, options,
	).Feasibility
	if verdict.Reason == unroll.ExitSweepBelowDust ||
		verdict.Reason == unroll.ExitUneconomical {

		e.InfeasibilityReason = verdict.Reason
		e.AdmissionError = unrollInfeasibleError(verdict).Error()
	}
}
