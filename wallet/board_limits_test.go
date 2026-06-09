package wallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/stretchr/testify/require"
)

// TestClampBoardingAmount exercises the pure clamp math that divides a
// confirmed boarding balance into a boardable amount, an on-chain
// change remainder, and a VTXO output count under the operator's
// advertised limits.
func TestClampBoardingAmount(t *testing.T) {
	t.Parallel()

	const (
		btc    = btcutil.Amount(100_000_000)
		maxV   = btcutil.Amount(10_000_000)
		dust   = btcutil.Amount(1_000)
		noMax  = btcutil.Amount(0)
		bigCap = btcutil.Amount(1 << 50)
	)

	tests := []struct {
		name        string
		total       btcutil.Amount
		targetCount uint32
		maxVTXO     btcutil.Amount
		headroom    btcutil.Amount
		wantBoard   btcutil.Amount
		wantChange  btcutil.Amount
		wantCount   uint32
		wantErr     error
	}{{
		// No limits bind: everything boards in one output.
		name:        "unbounded",
		total:       2 * btc,
		targetCount: 1,
		maxVTXO:     noMax,
		headroom:    bigCap,
		wantBoard:   2 * btc,
		wantChange:  0,
		wantCount:   1,
	}, {
		// The user's 2 BTC confirmed output boards only the cap
		// headroom; the remainder returns on-chain.
		name:        "headroom clips to change",
		total:       2 * btc,
		targetCount: 1,
		maxVTXO:     noMax,
		headroom:    btc,
		wantBoard:   btc,
		wantChange:  btc,
		wantCount:   1,
	}, {
		// The per-VTXO max splits the boarded amount into enough
		// outputs that each fits under the ceiling.
		name:        "per-vtxo max raises count",
		total:       25_000_000,
		targetCount: 1,
		maxVTXO:     maxV,
		headroom:    bigCap,
		wantBoard:   25_000_000,
		wantChange:  0,
		wantCount:   3,
	}, {
		// Both limits at once: clip to headroom, then split the
		// clipped amount under the per-VTXO max.
		name:        "headroom and per-vtxo max",
		total:       2 * btc,
		targetCount: 1,
		maxVTXO:     maxV,
		headroom:    15_000_000,
		wantBoard:   15_000_000,
		wantChange:  2*btc - 15_000_000,
		wantCount:   2,
	}, {
		// A requested output count above the per-VTXO minimum is
		// preserved.
		name:        "caller count preserved",
		total:       maxV,
		targetCount: 4,
		maxVTXO:     maxV,
		headroom:    bigCap,
		wantBoard:   maxV,
		wantChange:  0,
		wantCount:   4,
	}, {
		// A zero target count defaults to a single output.
		name:        "zero count defaults to one",
		total:       maxV,
		targetCount: 0,
		maxVTXO:     noMax,
		headroom:    bigCap,
		wantBoard:   maxV,
		wantChange:  0,
		wantCount:   1,
	}, {
		// A sub-dust remainder shifts back into the change output
		// so the on-chain remainder stays spendable.
		name:        "sub-dust change shifts board amount",
		total:       maxV + 500,
		targetCount: 1,
		maxVTXO:     noMax,
		headroom:    maxV,
		wantBoard:   maxV + 500 - dust,
		wantChange:  dust,
		wantCount:   1,
	}, {
		// No headroom at all: the cap is already reached.
		name:        "cap reached",
		total:       btc,
		targetCount: 1,
		maxVTXO:     noMax,
		headroom:    0,
		wantErr:     ErrBoardingCapReached,
	}, {
		// Negative headroom (already over the cap, e.g. after a
		// direct OOR receive) also rejects.
		name:        "over cap",
		total:       btc,
		targetCount: 1,
		maxVTXO:     noMax,
		headroom:    -btc,
		wantErr:     ErrBoardingCapReached,
	}, {
		// Headroom below the dust floor is unusable.
		name:        "headroom below floor",
		total:       btc,
		targetCount: 1,
		maxVTXO:     noMax,
		headroom:    dust - 1,
		wantErr:     ErrBoardingCapReached,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			clamp, err := clampBoardingAmount(
				tc.total, tc.targetCount, tc.maxVTXO,
				tc.headroom, dust,
			)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantBoard, clamp.BoardAmount)
			require.Equal(t, tc.wantChange, clamp.Change)
			require.Equal(t, tc.wantCount, clamp.VTXOCount)

			// The board/change split must conserve the total.
			require.Equal(
				t, tc.total, clamp.BoardAmount+clamp.Change,
			)

			// Splitting must yield outputs under the per-VTXO
			// max when one is set.
			amounts, err := splitBoardingAmount(
				clamp.BoardAmount, clamp.VTXOCount,
			)
			require.NoError(t, err)
			if tc.maxVTXO > 0 {
				for _, amt := range amounts {
					require.LessOrEqual(
						t, amt, tc.maxVTXO,
					)
				}
			}
		})
	}
}
