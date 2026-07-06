// Package coinselect provides a single, coin-type-agnostic coin-selection
// algorithm shared across the client. It deliberately holds no wallet,
// actor, or RPC dependencies so every layer that needs to pick a covering
// subset of coins — the VTXO manager's reservation path and the swap
// wallet's send preview alike — selects through the same code rather than
// growing parallel implementations.
package coinselect

import (
	"errors"
	"sort"

	"github.com/btcsuite/btcd/btcutil/v2"
)

// Selection errors. The selector stays policy-free: it reports why a pass
// failed and the covered total, and leaves layer-specific diagnostics (for
// example distinguishing locked liquidity from a true shortfall) to
// callers.
var (
	// ErrSelectionShortfall means the candidate set cannot cover the
	// requested target even when every candidate is selected. The
	// returned Result.Total carries the full candidate sum so callers
	// can render a precise message.
	ErrSelectionShortfall = errors.New("insufficient candidates to cover " +
		"target")

	// ErrChangeBelowMin means a covering selection exists but its
	// non-zero change falls below the requested minimum, and no
	// exact-fit (zero-change) selection was found. The returned
	// Result.Total carries the total at the first rejection point.
	ErrChangeBelowMin = errors.New("selection change below minimum")

	// ErrNoCandidates means selection was requested over an empty
	// candidate set.
	ErrNoCandidates = errors.New("no candidates to select")

	// ErrInvalidTarget means a bounded selection was requested with a
	// non-positive target.
	ErrInvalidTarget = errors.New("selection target must be positive")
)

// Request parameterizes a coin-selection pass over a homogeneous candidate
// set. Either Target (> 0) drives a bounded selection, or SweepAll selects
// every candidate; SweepAll takes precedence and ignores Target and
// MinChange.
type Request struct {
	// Target is the amount the selected candidates must cover. It is
	// required and must be positive unless SweepAll is set, in which
	// case it is ignored.
	Target btcutil.Amount

	// MinChange rejects a covering selection whose non-zero change is
	// below this floor, continuing the search for an exact-fit set.
	// Zero disables the constraint. An exact-fit selection
	// (change == 0) is always accepted regardless of MinChange. Ignored
	// when SweepAll is set.
	MinChange btcutil.Amount

	// SweepAll selects every candidate and ignores Target and MinChange.
	SweepAll bool
}

// Result is the outcome of a coin-selection pass. On success it carries the
// chosen subset; on the typed errors above it carries only the covered
// Total so callers can build precise diagnostics.
type Result[T any] struct {
	// Selected is the chosen subset (or every candidate for a sweep), in
	// selection order: largest-first for a bounded selection, input
	// order for a sweep.
	Selected []T

	// Total is the summed amount of Selected.
	Total btcutil.Amount

	// Change is Total - Target for a bounded selection. It is zero for a
	// sweep-all, where the caller owns the single output value.
	Change btcutil.Amount
}

// AmountFunc extracts the satoshi value of a candidate. Callers supply it
// so the selector stays agnostic to the concrete coin type (a VTXO
// Descriptor, an RPC VTXO, a boarding intent, and so on).
type AmountFunc[T any] func(T) btcutil.Amount

// LargestFirst runs largest-first coin selection. Candidates are sorted by
// descending amount and accumulated until the target is covered. When
// MinChange is set, a covering selection whose change would be non-zero but
// below MinChange is rejected in favour of accumulating further until the
// change clears MinChange; if no covering selection leaves change at or
// above MinChange, ErrChangeBelowMin is returned. An exact (zero-change)
// fit is always accepted. A SweepAll request selects every candidate
// regardless of Target.
//
// The caller's slice is never mutated; sorting happens on a copy. On
// failure the returned error is one of the typed errors in this file and
// the Result carries the relevant covered total.
func LargestFirst[T any](candidates []T, amount AmountFunc[T],
	req Request) (Result[T], error) {

	if req.SweepAll {
		return selectAll(candidates, amount)
	}

	if req.Target <= 0 {
		return Result[T]{}, ErrInvalidTarget
	}
	if len(candidates) == 0 {
		return Result[T]{}, ErrNoCandidates
	}

	// Sort a copy by descending amount so the caller's slice is left
	// untouched and selection is deterministic for distinct amounts.
	sorted := make([]T, len(candidates))
	copy(sorted, candidates)
	sort.SliceStable(sorted, func(i, j int) bool {
		return amount(sorted[i]) > amount(sorted[j])
	})

	var (
		total           btcutil.Amount
		rejectedTotal   btcutil.Amount
		rejectedForDust bool
	)

	// At most every candidate is selected, so size the slice up front to
	// avoid re-allocating its backing array as we accumulate.
	selected := make([]T, 0, len(sorted))
	for _, c := range sorted {
		selected = append(selected, c)
		total += amount(c)

		if total < req.Target {
			continue
		}

		// Accept an exact fit, or any change at or above the floor
		// (a zero floor disables the dust check entirely).
		change := total - req.Target
		if change == 0 || req.MinChange == 0 ||
			change >= req.MinChange {
			return Result[T]{
				Selected: selected,
				Total:    total,
				Change:   change,
			}, nil
		}

		// The selection covers the target but leaves dust change.
		// Remember the first such point and keep accumulating: adding
		// more value only grows the change, so a deeper selection may
		// clear MinChange (a dust-safe fit), though it can never return
		// to an exact zero-change fit.
		if !rejectedForDust {
			rejectedTotal = total
			rejectedForDust = true
		}
	}

	if rejectedForDust {
		return Result[T]{Total: rejectedTotal}, ErrChangeBelowMin
	}

	return Result[T]{Total: total}, ErrSelectionShortfall
}

// selectAll returns every candidate and their summed amount, modelling a
// sweep where no target or change applies.
func selectAll[T any](candidates []T, amount AmountFunc[T]) (Result[T], error) {
	if len(candidates) == 0 {
		return Result[T]{}, ErrNoCandidates
	}

	selected := make([]T, len(candidates))
	copy(selected, candidates)

	var total btcutil.Amount
	for _, c := range selected {
		total += amount(c)
	}

	return Result[T]{Selected: selected, Total: total}, nil
}
