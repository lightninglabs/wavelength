package wavewalletdk

import (
	"context"
	"errors"
)

// ErrActivityReplayUnavailable rejects bounded replay on a remote handle.
// The existing List and Subscribe RPCs remain available on those handles.
var ErrActivityReplayUnavailable = errors.New("canonical activity batches " +
	"require an embedded wallet")

// ActivityBatchRequest selects a finite transition range. Zero ThroughCursor
// captures a new end; reuse the returned HighWater until HasMore becomes false.
// Limit must be between one and 256. AfterCursor starts strictly after that
// transition. HighWater freezes the range, not the current projection values.
type ActivityBatchRequest struct {
	AfterCursor   int64 `json:"after_cursor"`
	ThroughCursor int64 `json:"through_cursor"`
	Limit         int32 `json:"limit"`
}

// ActivityBatch contains current canonical entries for a bounded replay range.
// Repeated transitions may name the same Entry.ID. A notification consumer
// deduplicates by canonical identity and persists NextCursor even when it
// filters entries. HasMore means another page remains at or below HighWater.
type ActivityBatch struct {
	HighWater  int64   `json:"high_water"`
	NextCursor int64   `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
	Entries    []Entry `json:"entries"`
}

// ReadActivityBatch reads current canonical projections without a live stream
// or unbounded history scan. It does not reconcile receipt sources or prove
// payment completion from a hint. Current projection states and host policy
// decide eligibility for presentation; this method only supplies bounded data.
func (c *Client) ReadActivityBatch(ctx context.Context,
	request ActivityBatchRequest) (*ActivityBatch, error) {

	if c == nil || c.readActivityBatch == nil {
		return nil, ErrActivityReplayUnavailable
	}
	callCtx, release, err := c.beginLocalCall(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	page, err := c.readActivityBatch(
		callCtx, request.AfterCursor, request.ThroughCursor,
		request.Limit,
	)
	if err != nil {
		return nil, err
	}
	result := &ActivityBatch{
		HighWater: page.HighWater, NextCursor: page.NextCursor,
		HasMore: page.More,
		Entries: make([]Entry, 0, len(page.Entries)),
	}
	for _, row := range page.Entries {
		entry, err := entryFromCanonicalActivity(row.Entry)
		if err != nil {
			return nil, err
		}
		entry.Cursor = row.Cursor
		result.Entries = append(result.Entries, entry)
	}

	return result, nil
}

// ActivityCursor captures a baseline for a new receipt consumer after the host
// has reconciled its canonical sources. It performs no reconciliation itself.
// It borrows the embedded store under the same shutdown owner as batch reads.
func (c *Client) ActivityCursor(ctx context.Context) (int64, error) {
	if c == nil || c.activityCursor == nil {
		return 0, ErrActivityReplayUnavailable
	}
	callCtx, release, err := c.beginLocalCall(ctx)
	if err != nil {
		return 0, err
	}
	defer release()

	return c.activityCursor(callCtx)
}
