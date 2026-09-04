package types

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/stretchr/testify/require"
)

// TestOperatorTermsMinVTXOAmountFloor verifies the effective VTXO floor
// never falls below the operator's dust limit.
func TestOperatorTermsMinVTXOAmountFloor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		terms *OperatorTerms
		want  btcutil.Amount
	}{{
		name:  "nil terms",
		terms: nil,
		want:  0,
	}, {
		name: "uses VTXO minimum above dust",
		terms: &OperatorTerms{
			DustLimit:     btcutil.Amount(546),
			MinVTXOAmount: btcutil.Amount(1234),
		},
		want: btcutil.Amount(1234),
	}, {
		name: "floors zero VTXO minimum at dust",
		terms: &OperatorTerms{
			DustLimit: btcutil.Amount(546),
		},
		want: btcutil.Amount(546),
	}, {
		name: "floors below-dust VTXO minimum at dust",
		terms: &OperatorTerms{
			DustLimit:     btcutil.Amount(546),
			MinVTXOAmount: btcutil.Amount(100),
		},
		want: btcutil.Amount(546),
	}, {
		name: "floors negative VTXO minimum at dust",
		terms: &OperatorTerms{
			DustLimit:     btcutil.Amount(546),
			MinVTXOAmount: btcutil.Amount(-1),
		},
		want: btcutil.Amount(546),
	}}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, tc.terms.MinVTXOAmountFloor())
		})
	}
}

// TestOperatorTermsVTXOTargetConfirmations pins the legacy fallback and the
// explicit split activation depth used by round confirmation watches.
func TestOperatorTermsVTXOTargetConfirmations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		terms *OperatorTerms
		want  uint32
	}{
		{
			name: "nil terms",
			want: 0,
		},
		{
			name: "legacy fallback",
			terms: &OperatorTerms{
				MinConfirmations: 6,
			},
			want: 6,
		},
		{
			name: "explicit activation depth",
			terms: &OperatorTerms{
				MinConfirmations:  6,
				VTXOConfirmations: 1,
			},
			want: 1,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, tc.want, tc.terms.VTXOTargetConfirmations(),
			)
		})
	}
}
