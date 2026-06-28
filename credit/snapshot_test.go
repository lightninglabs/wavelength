package credit

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOpSnapshotRoundTrip asserts the resume snapshot blob round-trips every
// field, including the credit-only marker the wallet projector reads.
func TestOpSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		snap *opSnapshot
	}{
		{
			name: "empty",
			snap: &opSnapshot{},
		},
		{
			name: "credit-only pay",
			snap: &opSnapshot{
				CreditOnly: true,
			},
		},
		{
			name: "receive with memo",
			snap: &opSnapshot{
				Memo: "coffee",
			},
		},
		{
			name: "redeem with pkscript",
			snap: &opSnapshot{
				RedeemPkScript: []byte{
					0x01,
					0x02,
					0x03,
				},
			},
		},
		{
			name: "all fields",
			snap: &opSnapshot{
				RedeemPkScript: []byte{
					0xaa,
					0xbb,
				},
				Memo:       "tea",
				CreditOnly: true,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw, err := tc.snap.encode()
			require.NoError(t, err)

			got, err := decodeOpSnapshot(raw)
			require.NoError(t, err)
			require.Equal(t, tc.snap.CreditOnly, got.CreditOnly)
			require.Equal(t, tc.snap.Memo, got.Memo)
			require.Equal(
				t, tc.snap.RedeemPkScript, got.RedeemPkScript,
			)
		})
	}
}
