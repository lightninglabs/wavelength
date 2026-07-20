package vtxo

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
)

// FilterOptions specifies the criteria for filtering a set of VTXO
// descriptors. A zero-value FilterOptions matches everything.
type FilterOptions struct {
	// Status filters to VTXOs matching this exact status. A zero
	// value (VTXOStatusLive) is treated as "no filter" only when
	// StatusSet is false.
	Status VTXOStatus

	// StatusSet indicates whether the Status field was explicitly
	// provided. This distinguishes "filter to live" from "no
	// filter", since VTXOStatusLive is the zero value.
	StatusSet bool

	// MinAmount filters to VTXOs with at least this amount in
	// satoshis. Zero means no minimum.
	MinAmount btcutil.Amount
}

// FilterDescriptors returns the subset of descriptors matching the
// given filter options. This is a pure function with no side effects,
// suitable for use by both RPC handlers and a future SDK.
func FilterDescriptors(descs []*Descriptor, opts FilterOptions) []*Descriptor {
	var result []*Descriptor

	for _, d := range descs {
		// Apply status filter if set.
		if opts.StatusSet && d.Status != opts.Status {
			continue
		}

		// Apply minimum amount filter.
		if opts.MinAmount > 0 && d.Amount < opts.MinAmount {
			continue
		}

		result = append(result, d)
	}

	return result
}

// SumBalance computes the total balance across a set of VTXO
// descriptors. This is a convenience function used by both the
// GetBalance RPC handler and the future SDK.
func SumBalance(descs []*Descriptor) btcutil.Amount {
	var total btcutil.Amount
	for _, d := range descs {
		total += d.Amount
	}

	return total
}

// SumSpendableBalance computes the total balance across only the
// spendable subset of descriptors. Only Live VTXOs can fund a spend;
// the other non-terminal states (PendingForfeit, Forfeiting, Spending)
// are still "live" for actor-recovery purposes but cannot back a spend,
// so they are excluded here. Callers reporting a spendable balance must
// use this rather than SumBalance over a recovery-oriented VTXO set,
// otherwise the figure overstates spendable liquidity.
func SumSpendableBalance(descs []*Descriptor) btcutil.Amount {
	spendable := FilterDescriptors(descs, FilterOptions{
		Status:    VTXOStatusLive,
		StatusSet: true,
	})

	return SumBalance(spendable)
}

// SumPendingBalance sums VTXOs in a non-terminal, non-spendable state
// (PendingForfeit, Forfeiting, Spending): the complement of
// SumSpendableBalance over the set ListLiveVTXOs returns. UnilateralExit
// is not in that set and is accounted for separately.
func SumPendingBalance(descs []*Descriptor) btcutil.Amount {
	var total btcutil.Amount
	for _, d := range descs {
		switch d.Status {
		case VTXOStatusPendingForfeit, VTXOStatusForfeiting,
			VTXOStatusSpending:

			total += d.Amount

		// Spendable, terminal, or separately accounted.
		case VTXOStatusLive, VTXOStatusForfeited, VTXOStatusSpent,
			VTXOStatusUnilateralExit, VTXOStatusFailed:
		}
	}

	return total
}

// ClassifyCanonicalityBalance separates lifecycle-live VTXOs into value that
// can be spent now and value temporarily blocked by lineage canonicality. A
// terminally invalidated lineage contributes to neither balance: it is no
// longer wallet liquidity and belongs in history or recovery diagnostics.
// A nil reader preserves the legacy behavior while the canonicality gate is
// disabled.
func ClassifyCanonicalityBalance(ctx context.Context, descs []*Descriptor,
	reader batchcanon.Reader) (btcutil.Amount, btcutil.Amount, error) {

	if reader == nil {
		return SumSpendableBalance(descs), 0, nil
	}

	var spendable, unavailable btcutil.Amount
	for _, desc := range descs {
		if desc == nil || desc.Status != VTXOStatusLive {
			continue
		}

		availability, err := batchcanon.LineageAvailability(
			ctx, reader, LineageCommitmentTxIDs(desc)...,
		)
		if err != nil {
			return 0, 0, fmt.Errorf("lineage availability for "+
				"%s: %w", desc.Outpoint, err)
		}

		switch {
		case availability.Usable():
			spendable += desc.Amount

		case availability != batchcanon.Invalidated:
			unavailable += desc.Amount
		}
	}

	return spendable, unavailable, nil
}
