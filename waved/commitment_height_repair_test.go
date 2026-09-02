package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestHasUnknownCommitmentHeight verifies that only usable ancestry with at
// least one missing height enters the startup repair.
func TestHasUnknownCommitmentHeight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		desc *vtxo.Descriptor
		want bool
	}{
		{
			name: "nil descriptor",
		},
		{
			name: "empty ancestry",
			desc: &vtxo.Descriptor{},
		},
		{
			name: "all heights known",
			desc: &vtxo.Descriptor{
				Ancestry: []vtxo.Ancestry{{
					CommitmentHeight: 100,
				}},
			},
		},
		{
			name: "zero height",
			desc: &vtxo.Descriptor{
				Ancestry: []vtxo.Ancestry{
					{},
				},
			},
			want: true,
		},
		{
			name: "mixed heights",
			desc: &vtxo.Descriptor{
				Ancestry: []vtxo.Ancestry{
					{
						CommitmentHeight: 100,
					},
					{},
				},
			},
			want: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(
				t, test.want,
				hasUnknownCommitmentHeight(test.desc),
			)
		})
	}
}
