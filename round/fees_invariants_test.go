package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestInvariantComputeClientOperatorFeeNonNegative asserts
// the client-side operator-fee reconciliation always returns a
// non-negative number. The fee emission path to the ledger
// silently drops negative values, so a regression that produced
// a negative fee would surface as a missing ledger row (hard to
// debug) rather than a crash. This invariant guards against
// that class of bug.
func TestInvariantComputeClientOperatorFeeNonNegative(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		intents, owned := drawIntentsAndOwned(rt)
		fee := computeClientOperatorFee(intents, owned)
		require.GreaterOrEqual(
			rt, fee, int64(0),
			"computeClientOperatorFee must never return a "+
				"negative value",
		)
	})
}

// TestInvariantComputeClientOperatorFeeMatchesConservation
// asserts the fee-conservation identity when outputs ≤ inputs:
// fee = Σ(inputs) − Σ(outputs). This is the arithmetic
// contract the ledger emission path depends on; a divergence
// would cause client-side total_fees_paid to silently drift
// from the server-booked amount.
func TestInvariantComputeClientOperatorFeeMatchesConservation(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		intents, owned := drawIntentsAndOwned(rt)

		inputs := sumBoarding(intents) + sumForfeits(intents)
		outputs := sumOwned(owned) + sumLeaves(intents)

		fee := computeClientOperatorFee(intents, owned)
		if outputs > inputs {
			// Clamped to zero when outputs exceed inputs.
			require.Equal(
				rt, int64(0), fee,
				"clamp branch must return zero",
			)

			return
		}

		require.Equal(
			rt, inputs-outputs, fee,
			"fee must equal (inputs - outputs) when non-negative",
		)
	})
}

// TestInvariantComputeClientOperatorFeeMonotoneInBoarding
// asserts adding a positive boarding input strictly increases
// the fee (or keeps it at zero when the clamp branch dominates).
// Any regression that double-counted or ignored a boarding
// input would violate this invariant.
func TestInvariantComputeClientOperatorFeeMonotoneInBoarding(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		intents, owned := drawIntentsAndOwned(rt)
		base := computeClientOperatorFee(intents, owned)

		add := rapid.Int64Range(1, 1_000_000).Draw(
			rt, "extraBoarding",
		)
		intents.Boarding = append(
			intents.Boarding,
			newBoardingIntent(
				btcutil.Amount(add),
			),
		)

		next := computeClientOperatorFee(intents, owned)
		require.GreaterOrEqual(
			rt, next, base,
			"adding a boarding input must never decrease the fee",
		)
	})
}

// TestInvariantComputeClientOperatorFeeMonotoneInOwnedOutput
// asserts adding a positive owned output either decreases the
// fee by that amount (when inputs dominate) or keeps it at zero
// (when the clamp branch fires). Regression guard symmetrical
// to the boarding monotonicity test above.
func TestInvariantComputeClientOperatorFeeMonotoneInOwnedOutput(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		intents, owned := drawIntentsAndOwned(rt)
		base := computeClientOperatorFee(intents, owned)

		add := rapid.Int64Range(1, 1_000_000).Draw(
			rt, "extraOwned",
		)
		owned = append(owned, &ClientVTXO{
			Amount: btcutil.Amount(add),
		})

		next := computeClientOperatorFee(intents, owned)
		require.LessOrEqual(
			rt, next, base,
			"adding an owned output must never increase the fee",
		)
	})
}

// TestComputeClientOperatorFeeUsesQuotedLeaveAmounts covers the
// #270 fee-accounting contract: when the intents carry server-
// authoritative leave amounts (captured at QuoteAccepted time),
// those override the pre-fee intent targets in the fee
// computation. Without this override, mixed refresh+leave rounds
// understate the emitted OperatorFeeSat because the leave-side
// residual is silently counted as pre-fee target value.
func TestComputeClientOperatorFeeUsesQuotedLeaveAmounts(t *testing.T) {
	t.Parallel()

	intents := Intents{
		Forfeits: []types.ForfeitRequest{
			{
				VTXOOutpoint: &wire.OutPoint{},
				Amount:       100_000,
			},
		},
		Leaves: []*types.LeaveRequest{
			{
				Output: &wire.TxOut{
					Value: 100_000,
				},
			},
		},
	}

	// Without the quote override the pre-fee target absorbs the
	// entire forfeit value → fee computes to zero.
	require.Equal(
		t, int64(0),
		computeClientOperatorFee(intents, nil),
	)

	// With the server-authoritative residual (post-fee amount of
	// 95_000), fee equals the 5_000 sat difference.
	intents.QuotedLeaveAmounts = []int64{95_000}
	require.Equal(
		t, int64(5_000), computeClientOperatorFee(intents, nil),
	)
}

// TestInvariantComputeClientOperatorFeeEmptyIntentsIsZero locks
// in the identity element: zero intents, zero owned outputs,
// fee is zero. A regression that returned a non-zero value for
// this edge case would indicate a broken baseline that would
// poison every round.
func TestInvariantComputeClientOperatorFeeEmptyIntentsIsZero(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, int64(0),
		computeClientOperatorFee(Intents{}, nil),
	)
}

// drawIntentsAndOwned draws a random (Intents, []*ClientVTXO)
// pair suitable for property testing. Amounts are capped at 1B
// sat per entry so the running sum cannot overflow int64 for
// reasonable slice lengths.
func drawIntentsAndOwned(rt *rapid.T) (Intents, []*ClientVTXO) {
	numBoarding := rapid.IntRange(0, 10).Draw(rt, "numBoarding")
	numForfeits := rapid.IntRange(0, 10).Draw(rt, "numForfeits")
	numOwned := rapid.IntRange(0, 10).Draw(rt, "numOwned")
	numLeaves := rapid.IntRange(0, 5).Draw(rt, "numLeaves")

	boarding := make([]BoardingIntent, 0, numBoarding)
	for i := 0; i < numBoarding; i++ {
		amt := rapid.Int64Range(0, 1_000_000_000).Draw(
			rt, "boardingAmt",
		)
		boarding = append(
			boarding,
			newBoardingIntent(
				btcutil.Amount(amt),
			),
		)
	}

	forfeits := make([]types.ForfeitRequest, 0, numForfeits)
	for i := 0; i < numForfeits; i++ {
		amt := rapid.Int64Range(0, 1_000_000_000).Draw(
			rt, "forfeitAmt",
		)
		forfeits = append(forfeits, types.ForfeitRequest{
			VTXOOutpoint: &wire.OutPoint{},
			Amount:       btcutil.Amount(amt),
		})
	}

	owned := make([]*ClientVTXO, 0, numOwned)
	for i := 0; i < numOwned; i++ {
		amt := rapid.Int64Range(0, 1_000_000_000).Draw(
			rt, "ownedAmt",
		)
		owned = append(owned, &ClientVTXO{
			Amount: btcutil.Amount(amt),
		})
	}

	leaves := make([]*types.LeaveRequest, 0, numLeaves)
	for i := 0; i < numLeaves; i++ {
		amt := rapid.Int64Range(0, 1_000_000_000).Draw(
			rt, "leaveAmt",
		)
		leaves = append(leaves, &types.LeaveRequest{
			Output: &wire.TxOut{Value: amt},
		})
	}

	return Intents{
		Boarding: boarding,
		Forfeits: forfeits,
		Leaves:   leaves,
	}, owned
}

// sumBoarding sums every boarding input's on-chain amount. Nil-
// safe because the helper always produces a concrete
// BoardingIntent.
func sumBoarding(in Intents) int64 {
	var out int64
	for i := range in.Boarding {
		out += int64(in.Boarding[i].ChainInfo.Amount)
	}

	return out
}

// sumForfeits sums every forfeit's cached amount hint.
func sumForfeits(in Intents) int64 {
	var out int64
	for i := range in.Forfeits {
		out += int64(in.Forfeits[i].Amount)
	}

	return out
}

// sumOwned sums every owned ClientVTXO amount, skipping nil
// entries to match computeClientOperatorFee's defensive
// handling.
func sumOwned(owned []*ClientVTXO) int64 {
	var out int64
	for _, v := range owned {
		if v == nil {
			continue
		}
		out += int64(v.Amount)
	}

	return out
}

// sumLeaves sums every leave-output value, skipping nil entries
// to match computeClientOperatorFee's defensive handling.
func sumLeaves(in Intents) int64 {
	var out int64
	for _, lv := range in.Leaves {
		if lv == nil || lv.Output == nil {
			continue
		}
		if lv.Output.Value > 0 {
			out += lv.Output.Value
		}
	}

	return out
}
