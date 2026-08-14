package tapassets

import (
	"fmt"
	"testing"

	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// testOwnedUtxo builds one candidate funding UTXO whose outpoint is derived
// from its label, so a fixture can name the UTXO a selection must pick.
func testOwnedUtxo(label string, amount uint64) ownedAssetUtxo {
	return ownedAssetUtxo{
		outpoint: tapsdk.Outpoint{
			Txid:  sha256Bytes([]byte(label)),
			Index: 0,
		},
		amount: amount,
	}
}

// selectedAmounts projects a selection onto its amounts in order.
func selectedAmounts(selected []ownedAssetUtxo) []uint64 {
	amounts := make([]uint64, len(selected))
	for idx := range selected {
		amounts[idx] = selected[idx].amount
	}

	return amounts
}

// TestOwnedAssetSelection pins the funding order onboarding selects its own
// tapd UTXOs in: an exact anchor first, then the smallest single anchor that
// covers the amount, then the smallest anchors accumulated until they do.
func TestOwnedAssetSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		candidates  []ownedAssetUtxo
		amount      uint64
		want        []uint64
		errContains string
	}{{
		// An exact anchor needs no change output, so it wins even
		// when a smaller accumulation would also cover the amount.
		name: "exact single anchor",
		candidates: []ownedAssetUtxo{
			testOwnedUtxo("a", 300),
			testOwnedUtxo("b", 500),
			testOwnedUtxo("c", 800),
		},
		amount: 500,
		want: []uint64{
			500,
		},
	}, {
		name: "smallest sufficient single anchor",
		candidates: []ownedAssetUtxo{
			testOwnedUtxo("a", 1_200),
			testOwnedUtxo("b", 800),
			testOwnedUtxo("c", 400),
		},
		amount: 500,
		want: []uint64{
			800,
		},
	}, {
		// No single anchor covers the amount, so the smallest ones
		// accumulate until they do and stop there.
		name: "accumulate smallest first",
		candidates: []ownedAssetUtxo{
			testOwnedUtxo("a", 400),
			testOwnedUtxo("b", 300),
			testOwnedUtxo("c", 200),
		},
		amount: 500,
		want: []uint64{
			200,
			300,
		},
	}, {
		name: "accumulate every anchor",
		candidates: []ownedAssetUtxo{
			testOwnedUtxo("a", 400),
			testOwnedUtxo("b", 300),
		},
		amount: 700,
		want: []uint64{
			300,
			400,
		},
	}, {
		name: "insufficient total",
		candidates: []ownedAssetUtxo{
			testOwnedUtxo("a", 400),
			testOwnedUtxo("b", 300),
		},
		amount:      900,
		errContains: "hold 700 units, need 900",
	}, {
		name:        "no candidate",
		amount:      100,
		errContains: "no owned UTXO holds the asset",
	}, {
		name: "amount required",
		candidates: []ownedAssetUtxo{
			testOwnedUtxo("a", 400),
		},
		errContains: "amount is required",
	}}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			selected, err := selectOwnedAssetUtxos(
				test.candidates, test.amount,
			)
			if test.errContains != "" {
				require.ErrorContains(t, err, test.errContains)
				require.Nil(t, selected)

				return
			}
			require.NoError(t, err)
			require.Equal(
				t, test.want, selectedAmounts(selected),
			)
		})
	}
}

// TestOwnedAssetSelectionIsDeterministic proves one wallet state always
// selects the same anchors regardless of the order tapd lists them, so a
// replay that has to re-resolve rebuilds the same transition.
func TestOwnedAssetSelectionIsDeterministic(t *testing.T) {
	t.Parallel()

	candidates := []ownedAssetUtxo{
		testOwnedUtxo("d", 200),
		testOwnedUtxo("c", 300),
		testOwnedUtxo("b", 200),
		testOwnedUtxo("a", 400),
	}
	selected, err := selectOwnedAssetUtxos(candidates, 500)
	require.NoError(t, err)

	// Ties on amount break on the outpoint, so the pair of 200s is fixed
	// rather than whichever one tapd happened to list first.
	require.Equal(t, []uint64{200, 200, 300}, selectedAmounts(selected))
	require.True(
		t, outpointBefore(selected[0].outpoint, selected[1].outpoint),
	)

	for shift := range candidates {
		rotated := make([]ownedAssetUtxo, 0, len(candidates))
		for idx := range candidates {
			rotated = append(
				rotated,
				candidates[(idx+shift)%len(candidates)],
			)
		}

		other, err := selectOwnedAssetUtxos(rotated, 500)
		require.NoError(t, err, "rotation %d", shift)
		require.Equal(t, selected, other, "rotation %d", shift)
	}
}

// TestOwnedAssetSelectionSpansEveryAnchor proves accumulation covers an
// amount no subset of the smaller anchors reaches on its own.
func TestOwnedAssetSelectionSpansEveryAnchor(t *testing.T) {
	t.Parallel()

	candidates := make([]ownedAssetUtxo, 0, 5)
	var total uint64
	for idx := range 5 {
		amount := uint64(100 * (idx + 1))
		total += amount
		candidates = append(
			candidates,
			testOwnedUtxo(
				fmt.Sprintf("utxo-%d", idx), amount,
			),
		)
	}

	selected, err := selectOwnedAssetUtxos(candidates, total)
	require.NoError(t, err)
	require.Len(t, selected, len(candidates))
	require.Equal(
		t, []uint64{100, 200, 300, 400, 500}, selectedAmounts(selected),
	)
}
