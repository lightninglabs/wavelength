package walletdk

import (
	"context"
	"fmt"
)

// ListBalance returns the wallet balance through the deprecated method name.
//
// Deprecated: use Balance.
func (c *Client) ListBalance(ctx context.Context) (*Balance, error) {
	return c.Balance(ctx)
}

// GetOnchainAddress returns a tracked deposit address through the deprecated
// method name.
//
// Deprecated: use Deposit.
func (c *Client) GetOnchainAddress(ctx context.Context) (*OnchainAddress,
	error) {

	resp, err := c.Deposit(ctx, DepositRequest{})
	if err != nil {
		return nil, err
	}

	return &OnchainAddress{Address: resp.Address}, nil
}

// ListSwaps returns send and receive wallet entries through the deprecated
// swap-shaped view.
//
// Deprecated: use List.
func (c *Client) ListSwaps(ctx context.Context, req ListSwapsRequest) (
	[]SwapSummary, error) {

	resp, err := c.List(ctx, ListRequest{
		PendingOnly: req.PendingOnly,
		Kinds: []EntryKind{
			EntryKindSend,
			EntryKindReceive,
		},
	})
	if err != nil {
		return nil, err
	}

	swaps := make([]SwapSummary, 0, len(resp.Entries))
	for _, entry := range resp.Entries {
		swaps = append(swaps, swapSummaryFromEntry(entry))
	}

	return swaps, nil
}

// GetSwap returns one send or receive wallet entry through the deprecated
// swap-shaped view.
//
// Deprecated: use List and match Entry.ID.
func (c *Client) GetSwap(ctx context.Context, req GetSwapRequest) (
	*SwapSummary, error) {

	swaps, err := c.ListSwaps(ctx, ListSwapsRequest{})
	if err != nil {
		return nil, err
	}

	for _, swap := range swaps {
		if swap.PaymentHash != req.PaymentHash {
			continue
		}

		return &swap, nil
	}

	return nil, fmt.Errorf("swap %s not found", req.PaymentHash)
}

// ResumeSwap is retained for callers compiled against the old walletdk API.
// Embedded walletdk now resumes pending swaps during daemon startup.
//
// Deprecated: startup resume is automatic in walletdk Start.
func (c *Client) ResumeSwap(ctx context.Context, req ResumeSwapRequest) (
	*SwapSummary, error) {

	return c.GetSwap(ctx, GetSwapRequest{
		PaymentHash: req.PaymentHash,
	})
}

// SubscribeSwaps streams send and receive wallet entries through the
// deprecated swap-shaped view.
//
// Deprecated: use Subscribe.
func (c *Client) SubscribeSwaps(ctx context.Context,
	req SubscribeSwapsRequest) (<-chan SwapSummary, <-chan error, error) {

	updates, errs, err := c.Subscribe(ctx, SubscribeRequest{
		IncludeExisting: req.IncludeExisting,
		Kinds: []EntryKind{
			EntryKindSend,
			EntryKindReceive,
		},
	})
	if err != nil {
		return nil, nil, err
	}

	swaps := make(chan SwapSummary)
	compatErrs := make(chan error, 1)
	go func() {
		defer close(swaps)
		defer close(compatErrs)

		for updates != nil || errs != nil {
			select {
			case entry, ok := <-updates:
				if !ok {
					updates = nil

					continue
				}
				if req.PendingOnly &&
					entry.Status != EntryStatusPending {

					continue
				}

				select {
				case swaps <- swapSummaryFromEntry(entry):
				case <-ctx.Done():
					compatErrs <- ctx.Err()

					return
				}

			case err, ok := <-errs:
				if !ok {
					errs = nil

					continue
				}
				if err != nil {
					compatErrs <- err

					return
				}

			case <-ctx.Done():
				compatErrs <- ctx.Err()

				return
			}
		}
	}()

	return swaps, compatErrs, nil
}
