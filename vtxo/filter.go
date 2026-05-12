package vtxo

import "github.com/btcsuite/btcd/btcutil"

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
