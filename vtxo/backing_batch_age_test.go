package vtxo

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateBackingBatchAge verifies the bounded-age policy accepts the
// inclusive boundary and fails closed for every missing, impossible, or
// already-expired backing-batch coordinate.
func TestValidateBackingBatchAge(t *testing.T) {
	t.Parallel()

	valid := &Descriptor{
		CreatedHeight: 100,
		BatchExpiry:   200,
	}

	tests := []struct {
		name          string
		desc          *Descriptor
		currentHeight int32
		maxAgeBlocks  uint32
		wantErr       string
	}{
		{
			name:         "zero limit preserves unknown metadata",
			desc:         nil,
			maxAgeBlocks: 0,
		},
		{
			name:          "younger than maximum",
			desc:          valid,
			currentHeight: 109,
			maxAgeBlocks:  10,
		},
		{
			name:          "equal to maximum",
			desc:          valid,
			currentHeight: 110,
			maxAgeBlocks:  10,
		},
		{
			name:          "older than maximum",
			desc:          valid,
			currentHeight: 111,
			maxAgeBlocks:  10,
			wantErr:       "age 11 exceeds maximum 10",
		},
		{
			name:          "unknown current height",
			desc:          valid,
			currentHeight: 0,
			maxAgeBlocks:  10,
			wantErr:       "current chain height 0 is unknown",
		},
		{
			name:          "missing descriptor",
			currentHeight: 110,
			maxAgeBlocks:  10,
			wantErr:       "descriptor is missing",
		},
		{
			name: "unknown creation height",
			desc: &Descriptor{
				BatchExpiry: 200,
			},
			currentHeight: 110,
			maxAgeBlocks:  10,
			wantErr:       "creation height 0 is unknown",
		},
		{
			name: "future creation height",
			desc: &Descriptor{
				CreatedHeight: 111,
				BatchExpiry:   200,
			},
			currentHeight: 110,
			maxAgeBlocks:  10,
			wantErr: "creation height 111 is above current " +
				"height 110",
		},
		{
			name: "unknown batch expiry",
			desc: &Descriptor{
				CreatedHeight: 100,
			},
			currentHeight: 110,
			maxAgeBlocks:  10,
			wantErr:       "expiry 0 is unknown or inconsistent",
		},
		{
			name: "expiry before creation",
			desc: &Descriptor{
				CreatedHeight: 100,
				BatchExpiry:   99,
			},
			currentHeight: 110,
			maxAgeBlocks:  10,
			wantErr:       "expiry 99 is unknown or inconsistent",
		},
		{
			name: "expiry at current height",
			desc: &Descriptor{
				CreatedHeight: 100,
				BatchExpiry:   110,
			},
			currentHeight: 110,
			maxAgeBlocks:  10,
			wantErr:       "expired at height 110",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := validateBackingBatchAge(
				test.desc, test.currentHeight,
				test.maxAgeBlocks,
			)
			if test.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, test.wantErr)
		})
	}
}
