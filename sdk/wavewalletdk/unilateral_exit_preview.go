package wavewalletdk

import (
	"context"
	"strings"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
)

// UnilateralExitPreview quotes a whole selection without authorizing work.
// Active jobs contribute their full proof cost. Refused and completed targets
// remain visible in Entries and contribute no fee. Change reuse alone does
// not predict extra delay or guarantee manual admission.
type UnilateralExitPreview struct {
	// SelectedAmountSat sums the face values of the selected VTXOs in
	// satoshis.
	SelectedAmountSat int64

	// EstimatedTransactions counts shared proof transactions once and
	// includes fee children and final spends.
	EstimatedTransactions uint32

	// FeeRateSatPerVByte is the satoshi-per-vbyte rate used for the
	// estimate.
	FeeRateSatPerVByte int64

	// EstimatedNetworkFeeSat includes both wallet-funded package fees and
	// fees deducted from BTC sweep outputs.
	EstimatedNetworkFeeSat int64

	// WalletFeeSat is the part of the network fee paid by the backing
	// wallet.
	WalletFeeSat int64

	// AdditionalFundingSat is the size of the suggested single deposit.
	// Zero means existing eligible coins and their confirmed change
	// suffice.
	AdditionalFundingSat int64

	// FundingAction describes whether the quote uses confirmed inputs,
	// reuses confirmed change or needs a deposit.
	FundingAction string

	// Entries retains request order and reports the outcome of each
	// selected outpoint.
	Entries []UnilateralExitTarget

	// SweepAddress is the validated custom destination, if requested.
	SweepAddress string
}

// UnilateralExitTarget reports the selected coin's deadline and current job.
type UnilateralExitTarget struct {
	// Outpoint identifies the selected VTXO by its transaction ID and
	// output index.
	Outpoint string

	// AmountSat is the face value of this VTXO before recovery fees.
	AmountSat int64

	// BatchExpiryHeight is the absolute block height at which the batch
	// expires.
	BatchExpiryHeight int32

	// ExitStatus reports the current unilateral-exit job state for the
	// target.
	ExitStatus ExitJobStatus

	// SweepTxid identifies the final sweep once the job has recorded it.
	SweepTxid string

	// LastError is the latest error reported by the existing exit job.
	LastError string

	// AdmissionError explains an economic refusal, such as a sweep below
	// dust.
	AdmissionError string

	// InfeasibilityReason identifies an economic refusal.
	InfeasibilityReason ExitInfeasibilityReason

	// RoundCommitment warns that a cooperative round holds this VTXO.
	// Manual recovery remains available.
	RoundCommitment string
}

// PreviewUnilateralExit fetches the read-only batch quote. An older server
// returns Unimplemented. The client never falls back to address allocation.
func (c *Client) PreviewUnilateralExit(ctx context.Context,
	req GetExitPlanRequest) (*UnilateralExitPreview, error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}
	resp, err := c.wallet.PreviewUnilateralExit(
		ctx, &wavewalletrpc.GetExitPlanRequest{
			Outpoints:          req.Outpoints,
			ConfTarget:         req.ConfTarget,
			SweepAddress:       req.SweepAddress,
			FeeRateSatPerVbyte: req.FeeRateSatPerVByte,
		},
	)
	if err != nil {
		return nil, err
	}

	result := &UnilateralExitPreview{
		SelectedAmountSat:      resp.GetSelectedAmountSat(),
		EstimatedTransactions:  resp.GetEstimatedTransactions(),
		FeeRateSatPerVByte:     resp.GetFeeRateSatPerVbyte(),
		EstimatedNetworkFeeSat: resp.GetEstimatedNetworkFeeSat(),
		WalletFeeSat:           resp.GetWalletFeeSat(),
		AdditionalFundingSat:   resp.GetAdditionalFundingSat(),
		SweepAddress:           resp.GetSweepAddress(),
		FundingAction: strings.ToLower(
			strings.TrimPrefix(
				resp.GetFundingAction().String(),
				"UNILATERAL_EXIT_FUNDING_ACTION_",
			),
		),
	}

	for _, entry := range resp.GetEntries() {
		result.Entries = append(result.Entries, UnilateralExitTarget{
			Outpoint:          entry.GetOutpoint(),
			AmountSat:         entry.GetAmountSat(),
			BatchExpiryHeight: entry.GetBatchExpiryHeight(),
			ExitStatus: exitJobStatusFromProto(
				entry.GetExitStatus(),
			),
			SweepTxid:      entry.GetSweepTxid(),
			LastError:      entry.GetLastError(),
			AdmissionError: entry.GetAdmissionError(),
			InfeasibilityReason: exitInfeasibilityReasonFromProto(
				entry.GetInfeasibilityReason(),
			),
			RoundCommitment: entry.GetRoundCommitment(),
		})
	}

	return result, nil
}
