package darepod

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResolveReorgSafetyDepth asserts the network-aware default and the
// explicit override for the reorg-safety / finality depth. An explicit
// positive value always wins; otherwise testnet gets the deeper default
// (its minimum-difficulty rule produces deep reorgs) and every other network
// gets the conventional six-block default. The resolved value is always
// positive so chain watches finalize rather than leak.
func TestResolveReorgSafetyDepth(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		network    string
		configured uint32
		want       uint32
	}{
		{
			name:    "mainnet default",
			network: "mainnet",
			want:    DefaultReorgSafetyDepth,
		},
		{
			name:    "regtest default",
			network: "regtest",
			want:    DefaultReorgSafetyDepth,
		},
		{
			name:    "signet default",
			network: "signet",
			want:    DefaultReorgSafetyDepth,
		},
		{
			name:    "testnet deeper default",
			network: "testnet",
			want:    DefaultTestnetReorgSafetyDepth,
		},
		{
			name:       "explicit override wins on testnet",
			network:    "testnet",
			configured: 30,
			want:       30,
		},
		{
			name:       "explicit override wins on mainnet",
			network:    "mainnet",
			configured: 50,
			want:       50,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := &Config{
				Network:          tc.network,
				ReorgSafetyDepth: tc.configured,
			}
			got := c.ResolveReorgSafetyDepth()
			require.Equal(t, tc.want, got)
			require.Positive(
				t, got, "resolved depth must be positive",
			)
		})
	}
}
