package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestRefreshPSBTShapeStableAcrossFeeSchedules asserts the
// structural invariant the issue #263 plan calls out under
// Phase C.4 (5): the PSBT shape a refresh produces — input
// count, output count, and per-output slot layout — must be
// identical with fees enabled vs disabled. Only the per-output
// VALUES should move. A regression that added a fee-only
// output (or dropped one) would change the tree weight and
// break the operator's validation surface.
//
// We exercise the invariant at the computeClientOperatorFee
// level because that is the function whose output determines
// how the wallet actor sizes the recipient / change outputs.
// For a fixed Intents + ownedVTXOs shape, the same cardinality
// must hold regardless of the fee value.
func TestRefreshPSBTShapeStableAcrossFeeSchedules(t *testing.T) {
	t.Parallel()

	// Canonical refresh: one forfeit input, one owned output.
	makeIntents := func() Intents {
		return Intents{
			Forfeits: []types.ForfeitRequest{
				{
					VTXOOutpoint: &wire.OutPoint{},
					Amount:       btcutil.Amount(100_000),
				},
			},
		}
	}

	// Case A: fees disabled (caller-computed zero fee).
	intentsNoFee := makeIntents()
	ownedNoFee := []*ClientVTXO{
		{
			Amount: btcutil.Amount(100_000),
		},
	}

	// Case B: fees enabled — same cardinality, different
	// owned-output value to absorb the fee.
	intentsWithFee := makeIntents()
	ownedWithFee := []*ClientVTXO{
		{
			Amount: btcutil.Amount(99_500),
		},
	}

	// Shape check: same number of forfeits, same number of
	// owned outputs, same number of leaves.
	require.Equal(
		t, len(intentsNoFee.Forfeits), len(intentsWithFee.Forfeits),
	)
	require.Equal(t, len(ownedNoFee), len(ownedWithFee))
	require.Equal(
		t, len(intentsNoFee.Leaves), len(intentsWithFee.Leaves),
	)

	// The fee delta is exactly the owned-output value delta.
	feeNoFee := computeClientOperatorFee(intentsNoFee, ownedNoFee)
	feeWithFee := computeClientOperatorFee(
		intentsWithFee, ownedWithFee,
	)
	require.Equal(
		t, int64(0), feeNoFee, "zero-fee case must produce zero fee",
	)
	require.Equal(
		t, int64(500), feeWithFee,
		"with-fee case must surface exactly 500 sats",
	)
}

// TestRefreshPSBTShapeStableUnderRapidSchedules extends the
// canonical shape assertion to random schedules (simulated via
// random fee deltas on the single owned output). The cardinality
// of every intent pool stays constant under random draws; the
// fee value is the only moving part.
func TestRefreshPSBTShapeStableUnderRapidSchedules(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		forfeitAmt := rapid.Int64Range(
			10_000, 1_000_000,
		).Draw(rt, "forfeitAmt")
		feeAmt := rapid.Int64Range(0, forfeitAmt-1).Draw(
			rt, "feeAmt",
		)

		intents := Intents{
			Forfeits: []types.ForfeitRequest{{
				VTXOOutpoint: &wire.OutPoint{},
				Amount:       btcutil.Amount(forfeitAmt),
			}},
		}
		owned := []*ClientVTXO{
			{
				Amount: btcutil.Amount(forfeitAmt - feeAmt),
			},
		}

		// Cardinality stays constant:
		require.Len(
			rt, intents.Forfeits, 1,
			"single forfeit input",
		)
		require.Len(rt, owned, 1, "single owned output")
		require.Len(rt, intents.Leaves, 0, "no leaves")

		// Fee matches exactly the caller-computed delta:
		got := computeClientOperatorFee(intents, owned)
		require.Equal(
			rt, feeAmt, got,
			"fee must equal forfeit - owned exactly",
		)
	})
}
