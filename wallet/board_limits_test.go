package wallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
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
		name          string
		total         btcutil.Amount
		targetCount   uint32
		maxVTXO       btcutil.Amount
		headroom      btcutil.Amount
		wantBoard     btcutil.Amount
		wantChange    btcutil.Amount
		wantCount     uint32
		wantDustToFee btcutil.Amount
		wantErr       error
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
	}, {
		// A confirmed balance below the floor with NO cap binding is
		// a floor problem, not a cap problem: it surfaces the distinct
		// ErrBoardAmountBelowFloor rather than a "max balance reached"
		// error.
		name:        "below floor no cap",
		total:       dust - 1,
		targetCount: 1,
		maxVTXO:     noMax,
		headroom:    bigCap,
		wantErr:     ErrBoardAmountBelowFloor,
	}, {
		// A per-VTXO maximum at the floor that needs more than
		// maxBoardOutputs outputs to cover the balance is rejected
		// rather than wrapping the count or over-allocating.
		name:        "too many outputs",
		total:       (maxBoardOutputs + 1) * dust,
		targetCount: 1,
		maxVTXO:     dust,
		headroom:    bigCap,
		wantErr:     ErrTooManyBoardOutputs,
	}, {
		// A per-VTXO maximum below the dust floor admits no valid VTXO
		// at all (every piece would be above the max or below the
		// floor), so the clamp rejects rather than minting guaranteed-
		// invalid outputs.
		name:        "max vtxo below floor",
		total:       2 * dust,
		targetCount: 1,
		maxVTXO:     dust - 1,
		headroom:    bigCap,
		wantErr:     ErrMaxVTXOBelowFloor,
	}, {
		// A requested count far above what the floor allows is capped
		// down so an even split keeps every piece at or above the
		// floor (here maxV / dust = 10000 pieces of exactly dust).
		name:        "count capped by floor",
		total:       maxV,
		targetCount: 1_000_000,
		maxVTXO:     noMax,
		headroom:    bigCap,
		wantBoard:   maxV,
		wantChange:  0,
		wantCount:   uint32(maxV / dust),
	}, {
		// A per-VTXO maximum within ~2x of the floor leaves an awkward
		// remainder that cannot form a second floor-clearing VTXO: one
		// full max-size piece boards and the sub-floor remainder is
		// dropped to the miner fee rather than minted as dust.
		name:          "infeasible split drops dust to fee",
		total:         3 * dust / 2,
		targetCount:   1,
		maxVTXO:       dust + dust/10,
		headroom:      bigCap,
		wantBoard:     dust + dust/10,
		wantChange:    0,
		wantCount:     1,
		wantDustToFee: 3*dust/2 - (dust + dust/10),
	}, {
		// A per-VTXO maximum exactly at the floor splits cleanly into
		// floor-sized pieces with nothing dropped to fee.
		name:        "max vtxo at floor splits cleanly",
		total:       3 * dust,
		targetCount: 1,
		maxVTXO:     dust,
		headroom:    bigCap,
		wantBoard:   3 * dust,
		wantChange:  0,
		wantCount:   3,
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
			require.Equal(t, tc.wantDustToFee, clamp.DustToFee)

			// Board, change, and the dust dropped to fee must
			// conserve the original confirmed balance.
			require.Equal(
				t, tc.total, clamp.BoardAmount+
					clamp.Change+clamp.DustToFee,
			)

			// Anything dropped to the miner fee is, by definition,
			// below the spendable floor.
			if clamp.DustToFee > 0 {
				require.Less(t, clamp.DustToFee, dust)
			}

			// Splitting must yield outputs within [floor, maxVTXO]:
			// the clamp picks a count that divides the boarded
			// amount into spendable, under-cap pieces.
			amounts, err := splitBoardingAmount(
				clamp.BoardAmount, clamp.VTXOCount,
			)
			require.NoError(t, err)
			for _, amt := range amounts {
				require.GreaterOrEqual(t, amt, dust)
				if tc.maxVTXO > 0 {
					require.LessOrEqual(t, amt, tc.maxVTXO)
				}
			}
		})
	}
}

// TestClampBoardingAmountInvariants fuzzes the boarding clamp + split over
// random operator limits and asserts the dust-safety invariants for every
// accepted board: value is conserved, no VTXO piece lands below the floor or
// above the per-VTXO maximum, and only a strictly-sub-floor remainder is ever
// dropped to the miner fee. We can credit sub-dust value elsewhere in the
// system (see the msat-credit work), but the boarding transaction itself must
// never mint a sub-dust output. A rejection is itself a valid outcome, so the
// invariants are asserted only on the accepted path.
func TestClampBoardingAmountInvariants(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		// Floor mirrors the production range: the dust limit / minimum
		// boarding amount, never below the P2TR floor.
		floor := btcutil.Amount(
			rapid.Int64Range(330, 10_000).Draw(rt, "floor"),
		)

		// Totals span sub-floor up to a few BTC so the awkward
		// near-floor regime is exercised alongside ordinary boards.
		total := btcutil.Amount(
			rapid.Int64Range(1, 5_000_000).Draw(rt, "total"),
		)

		// A maxVTXO of zero disables the per-VTXO cap; otherwise it
		// ranges from below the floor (rejected) through the awkward
		// near-floor band and up well past it.
		maxVTXO := btcutil.Amount(
			rapid.Int64Range(0, 2_000_000).Draw(rt, "maxVTXO"),
		)

		// Headroom spans over-cap (negative), binding, and effectively
		// unbounded.
		headroom := btcutil.Amount(
			rapid.Int64Range(
				-1_000_000, 1<<50).Draw(rt, "headroom"),
		)

		targetCount := rapid.Uint32Range(0, 4_000).Draw(rt, "count")

		clamp, err := clampBoardingAmount(
			total, targetCount, maxVTXO, headroom, floor,
		)
		if err != nil {

			// A rejection (cap reached, below floor, max below the
			// floor, too many outputs) is a valid outcome.
			return
		}

		// Conservation: every input satoshi is boarded, returned as
		// change, or dropped to the miner fee. Nothing is conjured or
		// lost.
		require.Equal(
			rt, total,
			clamp.BoardAmount+clamp.Change+clamp.DustToFee,
		)

		// Whatever falls to the miner fee is a true sub-floor dust
		// remainder: non-negative and strictly below the floor.
		require.GreaterOrEqual(rt, clamp.DustToFee, btcutil.Amount(0))
		require.Less(rt, clamp.DustToFee, floor)

		// The boarded amount and any change output are spendable: at or
		// above the floor (change is exactly zero when the full balance
		// boards).
		require.GreaterOrEqual(rt, clamp.BoardAmount, floor)
		if clamp.Change > 0 {
			require.GreaterOrEqual(rt, clamp.Change, floor)
		}

		// The board never exceeds the confirmed balance, nor the cap
		// headroom when the cap is the binding constraint.
		require.LessOrEqual(rt, clamp.BoardAmount, total)
		require.LessOrEqual(rt, clamp.BoardAmount, headroom)

		// The chosen count divides the boarded amount into spendable,
		// under-cap pieces that sum back to exactly the boarded amount.
		amounts, err := splitBoardingAmount(
			clamp.BoardAmount, clamp.VTXOCount,
		)
		require.NoError(rt, err)
		require.Len(rt, amounts, int(clamp.VTXOCount))

		var sum btcutil.Amount
		for _, amt := range amounts {
			require.GreaterOrEqual(rt, amt, floor)
			if maxVTXO > 0 {
				require.LessOrEqual(rt, amt, maxVTXO)
			}
			sum += amt
		}
		require.Equal(rt, clamp.BoardAmount, sum)
	})
}
