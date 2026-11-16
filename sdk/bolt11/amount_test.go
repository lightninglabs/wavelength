package bolt11

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

// TestAmountMSat proves an absent amount differs from zero, malformed amounts,
// and fixed invoices; caller amounts can never replace signed payment terms.
func TestAmountMSat(t *testing.T) {
	fixed := lnwire.MilliSatoshi(25_000_000)
	fraction := lnwire.MilliSatoshi(25_000_001)
	zero := lnwire.MilliSatoshi(0)
	for _, test := range []struct {
		name         string
		signed       *lnwire.MilliSatoshi
		caller, want uint64
		invalid      bool
	}{
		{
			name:   "payer chooses",
			caller: 25000,
			want:   25000000,
		},
		{
			name:   "one sat",
			caller: 1,
			want:   1000,
		},
		{
			name:    "missing payer amount",
			invalid: true,
		},
		{
			name:   "fixed amount",
			signed: &fixed,
			want:   25000000,
		},
		{
			name:   "same fixed amount",
			signed: &fixed,
			caller: 25000,
			want:   25000000,
		},
		{
			name:    "changed signed amount",
			signed:  &fixed,
			caller:  30000,
			invalid: true,
		},
		{
			name:   "fractional signed amount",
			signed: &fraction,
			want:   25000001,
		},
		{
			name:    "zero encoded amount",
			signed:  &zero,
			caller:  25000,
			invalid: true,
		},
		{
			name:   "supply bound",
			caller: 2100000000000000,
			want:   2100000000000000000,
		},
		{
			name:    "over supply",
			caller:  2100000000000001,
			invalid: true,
		},
		{
			name:    "uint64 overflow",
			caller:  ^uint64(0),
			invalid: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := AmountMSat(
				&zpay32.Invoice{
					MilliSat: test.signed,
				},
				test.caller,
			)
			if test.invalid {
				require.Error(t, err)

				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}
