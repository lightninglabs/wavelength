package wavewalletdk

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
)

// ExitBatchRequest selects exact inputs for one cooperative leave request.
// Destination is common to all inputs; empty allocates one wallet-owned
// address. There is no unilateral option or replay key.
type ExitBatchRequest struct {
	Outpoints   []string
	Destination string
}

// ExitBatchOutcome describes local admission, not confirmed payout.
type ExitBatchOutcome string

const (
	// ExitBatchUnknown requires reconciliation before any new submission.
	ExitBatchUnknown ExitBatchOutcome = "unknown"

	// ExitBatchAccepted means queued locally; the round can still fail.
	ExitBatchAccepted ExitBatchOutcome = "accepted"

	// ExitBatchFailed means this input was definitively not queued.
	ExitBatchFailed ExitBatchOutcome = "failed"
)

// ExitBatchInputResult preserves an input's admission status and explanation.
type ExitBatchInputResult struct {
	Outpoint string
	Outcome  ExitBatchOutcome
	Error    string
}

// ExitBatchResult contains one result per input in request order, and the
// common destination. Partial admission is possible. Inspect every result even
// when ExitBatch returns no error.
type ExitBatchResult struct {
	Results     []ExitBatchInputResult
	Destination string
	// ActivityID tracks the aggregate EXIT for the accepted subset only.
	ActivityID string
}

// ExitBatch submits one cooperative request without retry or unilateral
// fallback. InvalidArgument and FailedPrecondition reject before admission.
// Unimplemented means the daemon lacks this capability. Other RPC errors can
// hide accepted work: reconcile activity and input states before a new
// submission. No new operation identity is created by this method after an
// ambiguous response.
func (c *Client) ExitBatch(ctx context.Context, req ExitBatchRequest) (
	*ExitBatchResult, error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}
	if len(req.Outpoints) == 0 {
		return nil, fmt.Errorf("outpoints list is empty")
	}

	response, err := c.wallet.ExitBatch(ctx,
		&wavewalletrpc.ExitBatchRequest{
			Outpoints:      req.Outpoints,
			OnchainAddress: req.Destination,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("exit batch (do not retry without "+
			"reconciling admission): %w", err)
	}

	result := &ExitBatchResult{
		Destination: response.GetOnchainAddress(),
		ActivityID:  response.GetActivityId(),
	}
	for _, input := range response.GetResults() {
		outcome := ExitBatchUnknown
		switch input.GetOutcome() {
		case wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_ACCEPTED:
			outcome = ExitBatchAccepted

		case wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_FAILED:
			outcome = ExitBatchFailed

		case wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_UNKNOWN:
			outcome = ExitBatchUnknown
		}
		result.Results = append(result.Results, ExitBatchInputResult{
			Outpoint: input.GetOutpoint(),
			Outcome:  outcome,
			Error:    input.GetError(),
		})
	}

	return result, nil
}
