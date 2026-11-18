package arkchannel

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSourceMaterializationHeight pins the reserve and rejects unknown or
// impossible deadlines without allowing either signed or unsigned wraparound.
func TestSourceMaterializationHeight(t *testing.T) {
	t.Parallel()

	require.Equal(t, 40, SourceExpirySafetyMargin)
	tests := []struct {
		name      string
		expiry    int32
		allowance uint32
		deadline  int32
		invalid   bool
	}{
		{
			name: "full reserve", expiry: 1000, allowance: 100,
			deadline: 860,
		},
		{
			name: "minimum deadline", expiry: 42, allowance: 1,
			deadline: 1,
		},
		{
			name: "maximum expiry", expiry: math.MaxInt32,
			allowance: 1, deadline: math.MaxInt32 - 41,
		},
		{
			name:   "maximum reserve with deadline",
			expiry: math.MaxInt32, allowance: math.MaxInt32 - 41,
			deadline: 1,
		},
		{
			name: "absent expiry", allowance: 1, invalid: true,
		},
		{
			name: "negative expiry", expiry: -1, allowance: 1,
			invalid: true,
		},
		{
			name: "minimum expiry", expiry: math.MinInt32,
			allowance: 1, invalid: true,
		},
		{
			name: "zero allowance", expiry: 1000, invalid: true,
		},
		{
			name: "zero deadline", expiry: 140, allowance: 100,
			invalid: true,
		},
		{
			name: "negative deadline", expiry: 139, allowance: 100,
			invalid: true,
		},
		{
			name: "signed reserve overflow", expiry: math.MaxInt32,
			allowance: math.MaxInt32, invalid: true,
		},
		{
			name:      "unsigned reserve overflow",
			expiry:    math.MaxInt32,
			allowance: math.MaxUint32, invalid: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			deadline, err := SourceMaterializationHeight(
				test.expiry, test.allowance,
			)
			if test.invalid {
				require.Error(t, err)
				require.Zero(t, deadline)

				return
			}

			require.NoError(t, err)
			require.Equal(t, test.deadline, deadline)
		})
	}
}

// TestValidateSourceLifetime requires strict admission before the computed
// deadline and never treats a negative backend height as usable chain state.
func TestValidateSourceLifetime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		expiry    int32
		height    int32
		allowance uint32
		invalid   bool
	}{
		{
			name: "genesis", expiry: 1000, allowance: 100,
		},
		{
			name: "before deadline", expiry: 1000, height: 859,
			allowance: 100,
		},
		{
			name: "at deadline", expiry: 1000, height: 860,
			allowance: 100, invalid: true,
		},
		{
			name: "after deadline", expiry: 1000, height: 861,
			allowance: 100, invalid: true,
		},
		{
			name: "expired", expiry: 1000, height: 1000,
			allowance: 100, invalid: true,
		},
		{
			name: "negative height", expiry: 1000, height: -1,
			allowance: 100, invalid: true,
		},
		{
			name: "unknown expiry", height: 1, allowance: 100,
			invalid: true,
		},
		{
			name: "unknown allowance", expiry: 1000, height: 1,
			invalid: true,
		},
		{
			name: "overflow", expiry: math.MaxInt32, height: 1,
			allowance: math.MaxUint32, invalid: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateSourceLifetime(
				test.expiry, test.height, test.allowance,
			)
			if test.invalid {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
		})
	}
}
