package oor

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaprootAssetTransferRoundTrip(t *testing.T) {
	t.Parallel()

	original := &TaprootAssetTransfer{
		Version: TaprootAssetTransferVersion,
		CheckpointPackages: [][]byte{
			[]byte("checkpoint-0"),
			[]byte("checkpoint-1"),
		},
		ArkPackage: []byte("ark"),
	}

	require.NoError(t, original.Validate(2))
	encoded, err := original.MarshalBinary()
	require.NoError(t, err)

	var decoded TaprootAssetTransfer
	require.NoError(t, decoded.UnmarshalBinary(encoded))
	require.Equal(t, original, &decoded)
	require.NoError(t, decoded.Validate(2))

	clone := decoded.Clone()
	clone.CheckpointPackages[0][0] ^= 1
	clone.ArkPackage[0] ^= 1
	require.NotEqual(
		t, clone.CheckpointPackages[0], decoded.CheckpointPackages[0],
	)
	require.NotEqual(t, clone.ArkPackage, decoded.ArkPackage)
}

func TestTaprootAssetTransferRejectsInvalidContainers(t *testing.T) {
	t.Parallel()

	valid := &TaprootAssetTransfer{
		Version: TaprootAssetTransferVersion,
		CheckpointPackages: [][]byte{
			[]byte("checkpoint"),
		},
		ArkPackage: []byte("ark"),
	}
	encoded, err := valid.MarshalBinary()
	require.NoError(t, err)

	tests := []struct {
		name    string
		value   *TaprootAssetTransfer
		count   int
		target  error
		wantErr string
	}{
		{
			name:   "nil",
			count:  -1,
			target: ErrTaprootAssetTransferInvalid,
		},
		{
			name: "version",
			value: &TaprootAssetTransfer{
				Version: 99,
				CheckpointPackages: [][]byte{
					[]byte("checkpoint"),
				},
				ArkPackage: []byte("ark"),
			},
			count:  -1,
			target: ErrTaprootAssetTransferVersion,
		},
		{
			name: "missing checkpoints",
			value: &TaprootAssetTransfer{
				Version:    TaprootAssetTransferVersion,
				ArkPackage: []byte("ark"),
			},
			count:   -1,
			target:  ErrTaprootAssetTransferInvalid,
			wantErr: "checkpoint packages are required",
		},
		{
			name:   "count mismatch",
			value:  valid,
			count:  2,
			target: ErrTaprootAssetTransferInvalid,
		},
		{
			name: "empty checkpoint",
			value: &TaprootAssetTransfer{
				Version: TaprootAssetTransferVersion,
				CheckpointPackages: [][]byte{
					nil,
				},
				ArkPackage: []byte("ark"),
			},
			count:  -1,
			target: ErrTaprootAssetTransferInvalid,
		},
		{
			name: "empty ark",
			value: &TaprootAssetTransfer{
				Version: TaprootAssetTransferVersion,
				CheckpointPackages: [][]byte{
					[]byte("checkpoint"),
				},
			},
			count:  -1,
			target: ErrTaprootAssetTransferInvalid,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.value.Validate(test.count)
			require.ErrorIs(t, err, test.target)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
			}
		})
	}

	corrupt := append([]byte(nil), encoded...)
	corrupt[len(corrupt)-1] ^= 1
	var decoded TaprootAssetTransfer
	err = decoded.UnmarshalBinary(corrupt)
	require.ErrorIs(t, err, ErrTaprootAssetTransferInvalid)
	require.ErrorContains(t, err, "checksum")

	unknownVersion := append([]byte(nil), encoded...)
	unknownVersion[len(taprootAssetTransferMagic)+1] = 1
	// The checksum must be invalid too, so this should still fail closed.
	err = decoded.UnmarshalBinary(unknownVersion)
	require.True(
		t, errors.Is(err, ErrTaprootAssetTransferInvalid) ||
			errors.Is(err, ErrTaprootAssetTransferVersion),
	)
}
