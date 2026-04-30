package indexer

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNarrowVBytesTotalBoundary verifies the saturation guard around
// the uint32 narrowing used by the cap-arithmetic surface. Saturating
// at MaxUint32 would silently mask real over-cap lineages when an
// operator runs with `MaxOORLineageVBytes == MaxUint32` ("no cap"); the
// helper must fail closed instead.
func TestNarrowVBytesTotalBoundary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		total     uint64
		want      uint32
		expectErr bool
	}{
		{
			name:  "zero passes through",
			total: 0,
			want:  0,
		},
		{
			name:  "small value passes through",
			total: 25_000,
			want:  25_000,
		},
		{
			name:  "exactly MaxUint32 still narrows",
			total: math.MaxUint32,
			want:  math.MaxUint32,
		},
		{
			name:      "MaxUint32+1 errors",
			total:     uint64(math.MaxUint32) + 1,
			expectErr: true,
		},
		{
			name:      "far above MaxUint32 errors",
			total:     uint64(math.MaxUint32) * 4,
			expectErr: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := narrowVBytesTotal(tc.total)
			if tc.expectErr {
				require.Error(t, err)
				require.Contains(t, err.Error(),
					"lineage vbytes overflow")
				require.Contains(t, err.Error(),
					"exceeds uint32 max")

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
