package tapassets

import (
	"context"
	"errors"
	"fmt"

	tapsdk "github.com/lightninglabs/tap-sdk"
)

// Publish verifies the exact finalized anchor PSBT against the sealed
// package and hands both to tapd, completing the backend's transfer
// bookkeeping: the funding UTXO leaves the inventory, the batch output
// joins it, and proofs generate on confirmation. The commit recorded
// external broadcast, so tapd logs without broadcasting.
func (c *BatchAnchorCommitter) Publish(ctx context.Context, packageBytes,
	finalPSBT []byte) error {

	if c == nil {
		return fmt.Errorf("batch anchor committer is required")
	}
	if len(packageBytes) == 0 || len(finalPSBT) == 0 {
		return fmt.Errorf("sealed package and final anchor PSBT are " +
			"required")
	}

	sdk, ok := c.driver.(*sdkDriver)
	if !ok {
		return fmt.Errorf("publish requires the tap-sdk driver")
	}

	var transfer tapsdk.CustomAnchorTransferPackage
	if err := transfer.UnmarshalBinary(packageBytes); err != nil {
		return fmt.Errorf("decode sealed transfer package: %w", err)
	}
	if err := transfer.VerifyFinalAnchorPSBT(finalPSBT); err != nil {
		return fmt.Errorf("verify final anchor PSBT: %w", err)
	}

	_, err := sdk.wallet.PublishCustomAnchorTransfer(
		ctx, &transfer, finalPSBT,
	)
	if err != nil {
		var attemptErr *tapsdk.CustomAnchorPublishAttemptError
		if errors.As(err, &attemptErr) && attemptErr.OutcomeUnknown {
			return errors.Join(
				ErrReconciliationRequired,
				fmt.Errorf("publish batch anchor transfer: %w",
					err),
			)
		}

		return fmt.Errorf("publish batch anchor transfer: %w", err)
	}

	return nil
}
