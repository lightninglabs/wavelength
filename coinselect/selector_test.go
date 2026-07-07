package coinselect

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/stretchr/testify/require"
)

// coin is a minimal candidate type for exercising the selector without
// pulling in any wallet types.
type coin struct {
	id     int
	amount btcutil.Amount
}

// coinAmount is the AmountFunc for the test coin type.
func coinAmount(c coin) btcutil.Amount {
	return c.amount
}

// coins builds a candidate slice from the given amounts, assigning each a
// distinct id so selection identity is observable.
func coins(amounts ...btcutil.Amount) []coin {
	out := make([]coin, len(amounts))
	for i, a := range amounts {
		out[i] = coin{id: i, amount: a}
	}

	return out
}

// TestLargestFirstBounded exercises bounded largest-first selection:
// ordering, exact fit, multi-input coverage, and the dust-change search.
func TestLargestFirstBounded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		amounts   []btcutil.Amount
		target    btcutil.Amount
		minChange btcutil.Amount
		wantCount int
		wantTotal btcutil.Amount
		wantErr   error
	}{
		{
			name: "single covers target",
			amounts: []btcutil.Amount{
				50000,
				30000,
				10000,
			},
			target:    40000,
			wantCount: 1,
			wantTotal: 50000,
		},
		{
			name: "largest first picks biggest",
			amounts: []btcutil.Amount{
				10000,
				50000,
				30000,
			},
			target:    45000,
			wantCount: 1,
			wantTotal: 50000,
		},
		{
			name: "two inputs needed",
			amounts: []btcutil.Amount{
				30000,
				25000,
				10000,
			},
			target:    50000,
			wantCount: 2,
			wantTotal: 55000,
		},
		{
			name: "exact fit zero change",
			amounts: []btcutil.Amount{
				50000,
			},
			target:    50000,
			wantCount: 1,
			wantTotal: 50000,
		},
		{
			name: "shortfall",
			amounts: []btcutil.Amount{
				20000,
				10000,
			},
			target:    50000,
			wantTotal: 30000,
			wantErr:   ErrSelectionShortfall,
		},
		{
			name:    "empty candidates",
			amounts: nil,
			target:  1000,
			wantErr: ErrNoCandidates,
		},
		{
			name: "non-positive target",
			amounts: []btcutil.Amount{
				1000,
			},
			target:  0,
			wantErr: ErrInvalidTarget,
		},
		{
			// 50000 covers 49900 but leaves 100 change, below the
			// 1000 floor; no deeper exact fit exists, so the pass
			// fails with the rejected total surfaced.
			name: "dust change rejected no exact fit",
			amounts: []btcutil.Amount{
				50000,
			},
			target:    49900,
			minChange: 1000,
			wantTotal: 50000,
			wantErr:   ErrChangeBelowMin,
		},
		{
			// 30000 leaves dust change for target 30100, but adding
			// the 100 yields an exact (zero-change) fit, which is
			// always accepted.
			name: "dust rejected then exact fit found",
			amounts: []btcutil.Amount{
				30000,
				100,
			},
			target:    30100,
			minChange: 1000,
			wantCount: 2,
			wantTotal: 30100,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res, err := LargestFirst(
				coins(tc.amounts...), coinAmount, Request{
					Target:    tc.target,
					MinChange: tc.minChange,
				},
			)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.Nil(t, res.Selected)
				require.Equal(t, tc.wantTotal, res.Total)

				return
			}

			require.NoError(t, err)
			require.Len(t, res.Selected, tc.wantCount)
			require.Equal(t, tc.wantTotal, res.Total)
			require.Equal(t, tc.wantTotal-tc.target, res.Change)
			require.GreaterOrEqual(
				t, int64(res.Total), int64(tc.target),
			)
		})
	}
}

// TestLargestFirstSweepAll verifies sweep-all selects every candidate, sums
// them, and reports zero change regardless of input order.
func TestLargestFirstSweepAll(t *testing.T) {
	t.Parallel()

	res, err := LargestFirst(
		coins(10000, 50000, 30000), coinAmount, Request{
			SweepAll: true,
		},
	)
	require.NoError(t, err)
	require.Len(t, res.Selected, 3)
	require.Equal(t, btcutil.Amount(90000), res.Total)
	require.Equal(t, btcutil.Amount(0), res.Change)
}

// TestSweepAllEmpty verifies a sweep over no candidates is an error, never a
// silent empty success.
func TestSweepAllEmpty(t *testing.T) {
	t.Parallel()

	_, err := LargestFirst(coins(), coinAmount, Request{SweepAll: true})
	require.ErrorIs(t, err, ErrNoCandidates)
}

// TestLargestFirstDoesNotMutateInput guards the contract that the caller's
// candidate slice ordering is never disturbed by the internal sort.
func TestLargestFirstDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	candidates := coins(10000, 50000, 30000)
	before := append([]coin(nil), candidates...)

	_, err := LargestFirst(
		candidates, coinAmount, Request{
			Target: 40000,
		},
	)
	require.NoError(t, err)
	require.Equal(t, before, candidates)
}
