package oor

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTaprootAssetTransferRoundTrip verifies binary encoding, validation,
// cloning, and empty non-asset checkpoint slots.
func TestTaprootAssetTransferRoundTrip(t *testing.T) {
	t.Parallel()

	transfer := &TaprootAssetTransfer{
		Version: TaprootAssetTransferVersion,
		CheckpointPackages: [][]byte{
			[]byte("checkpoint-asset-0"),
			nil,
			[]byte("checkpoint-asset-2"),
		},
		ArkPackage: []byte("ark-asset-package"),
	}

	require.NoError(t, transfer.Validate(3))
	require.ErrorIs(t, transfer.Validate(2), ErrTaprootAssetTransferInvalid)

	encoded, err := transfer.MarshalBinary()
	require.NoError(t, err)

	var decoded TaprootAssetTransfer
	require.NoError(t, decoded.UnmarshalBinary(encoded))
	require.Equal(t, *transfer, decoded)

	cloned := transfer.Clone()
	require.Equal(t, transfer, cloned)
	cloned.CheckpointPackages[0][0] ^= 0xff
	cloned.ArkPackage[0] ^= 0xff
	require.Equal(
		t, []byte("checkpoint-asset-0"), transfer.CheckpointPackages[0],
	)
	require.Equal(t, []byte("ark-asset-package"), transfer.ArkPackage)
	require.Nil(t, (*TaprootAssetTransfer)(nil).Clone())
}

// TestTaprootAssetTransferRejectsMalformed verifies version, magic, checksum,
// framing, and semantic bounds on decode and validate.
func TestTaprootAssetTransferRejectsMalformed(t *testing.T) {
	t.Parallel()

	valid := &TaprootAssetTransfer{
		Version: TaprootAssetTransferVersion,
		CheckpointPackages: [][]byte{
			[]byte("cp-0"),
		},
		ArkPackage: []byte("ark-0"),
	}
	encoded, err := valid.MarshalBinary()
	require.NoError(t, err)

	withRecomputedChecksum := func(body []byte) []byte {
		sum := sha256.Sum256(body)
		out := append([]byte(nil), body...)

		return append(out, sum[:]...)
	}

	bodyLen := len(encoded) - taprootAssetTransferChecksumSize
	baseBody := encoded[:bodyLen]

	tests := []struct {
		name    string
		raw     []byte
		wantErr error
	}{
		{
			name:    "truncated",
			raw:     encoded[:taprootAssetTransferHeaderSize],
			wantErr: ErrTaprootAssetTransferInvalid,
		},
		{
			name: "bad checksum",
			raw: func() []byte {
				b := bytes.Clone(encoded)
				b[len(b)-1] ^= 0x01

				return b
			}(),
			wantErr: ErrTaprootAssetTransferInvalid,
		},
		{
			name: "bad magic",
			raw: func() []byte {
				b := bytes.Clone(baseBody)
				b[0] = 'X'

				return withRecomputedChecksum(b)
			}(),
			wantErr: ErrTaprootAssetTransferInvalid,
		},
		{
			name: "unsupported version",
			raw: func() []byte {
				b := bytes.Clone(baseBody)
				binary.BigEndian.PutUint16(
					b[len(taprootAssetTransferMagic):], 9,
				)

				return withRecomputedChecksum(b)
			}(),
			wantErr: ErrTaprootAssetTransferVersion,
		},
		{
			name: "zero checkpoint count",
			raw: func() []byte {
				b := bytes.Clone(baseBody)
				binary.BigEndian.PutUint16(
					b[len(taprootAssetTransferMagic)+2:], 0,
				)

				return withRecomputedChecksum(b)
			}(),
			wantErr: ErrTaprootAssetTransferInvalid,
		},
		{
			name: "trailing bytes",
			raw: func() []byte {
				b := append(bytes.Clone(baseBody), 0xaa)

				return withRecomputedChecksum(b)
			}(),
			wantErr: ErrTaprootAssetTransferInvalid,
		},
		{
			name: "no asset checkpoint packages",
			raw: func() []byte {
				var b bytes.Buffer
				b.WriteString(taprootAssetTransferMagic)
				require.NoError(
					t, binary.Write(
						&b, binary.BigEndian,
						TaprootAssetTransferVersion,
					),
				)
				require.NoError(
					t,
					binary.Write(
						&b, binary.BigEndian, uint16(1),
					),
				)
				require.NoError(
					t,
					binary.Write(
						&b, binary.BigEndian, uint32(0),
					),
				)
				require.NoError(
					t,
					writeTaprootAssetPackage(
						&b, []byte("ark"),
					),
				)

				return withRecomputedChecksum(b.Bytes())
			}(),
			wantErr: ErrTaprootAssetTransferInvalid,
		},
		{
			name: "declared length exceeds remaining bytes",
			raw: func() []byte {
				var b bytes.Buffer
				b.WriteString(taprootAssetTransferMagic)
				require.NoError(
					t, binary.Write(
						&b, binary.BigEndian,
						TaprootAssetTransferVersion,
					),
				)
				require.NoError(
					t,
					binary.Write(
						&b, binary.BigEndian, uint16(1),
					),
				)
				require.NoError(
					t,
					binary.Write(
						&b, binary.BigEndian,
						uint32(1024),
					),
				)
				b.WriteString("short")

				return withRecomputedChecksum(b.Bytes())
			}(),
			wantErr: ErrTaprootAssetTransferInvalid,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out TaprootAssetTransfer
			err := out.UnmarshalBinary(tc.raw)
			require.ErrorIs(t, err, tc.wantErr)
		})
	}

	require.ErrorIs(
		t, (*TaprootAssetTransfer)(nil).UnmarshalBinary(encoded),
		ErrTaprootAssetTransferInvalid,
	)
	require.ErrorIs(
		t, (&TaprootAssetTransfer{
			Version:            TaprootAssetTransferVersion,
			CheckpointPackages: [][]byte{nil},
			ArkPackage:         []byte("ark"),
		}).Validate(-1),
		ErrTaprootAssetTransferInvalid,
	)
	require.ErrorIs(
		t, (&TaprootAssetTransfer{
			Version:            TaprootAssetTransferVersion,
			CheckpointPackages: [][]byte{[]byte("cp")},
		}).Validate(-1),
		ErrTaprootAssetTransferInvalid,
	)

	// Framing overhead counts against MaxTaprootAssetTransferBytes in both
	// Validate and MarshalBinary so a transfer cannot pass Validate and
	// then fail MarshalBinary.
	framing := taprootAssetTransferHeaderSize + 2*4 +
		taprootAssetTransferChecksumSize
	atCap := &TaprootAssetTransfer{
		Version: TaprootAssetTransferVersion,
		CheckpointPackages: [][]byte{
			bytes.Repeat([]byte{0x01}, MaxTaprootAssetPackageBytes),
		},
		ArkPackage: bytes.Repeat(
			[]byte{0x02}, MaxTaprootAssetTransferBytes-
				MaxTaprootAssetPackageBytes-framing,
		),
	}
	require.NoError(t, atCap.Validate(1))
	capEncoded, err := atCap.MarshalBinary()
	require.NoError(t, err)
	require.Len(t, capEncoded, MaxTaprootAssetTransferBytes)

	overByOne := atCap.Clone()
	overByOne.ArkPackage = append(overByOne.ArkPackage, 0x03)
	require.ErrorIs(
		t, overByOne.Validate(1), ErrTaprootAssetTransferInvalid,
	)
	_, err = overByOne.MarshalBinary()
	require.ErrorIs(t, err, ErrTaprootAssetTransferInvalid)

	// Bad magic and unsupported version are rejected before checksum
	// verification, even when the trailing checksum bytes are bogus.
	badMagicBogusChecksum := bytes.Clone(encoded)
	badMagicBogusChecksum[0] = 'X'
	badMagicBogusChecksum[len(badMagicBogusChecksum)-1] ^= 0xff
	var decoded TaprootAssetTransfer
	err = decoded.UnmarshalBinary(badMagicBogusChecksum)
	require.ErrorIs(t, err, ErrTaprootAssetTransferInvalid)
	require.ErrorContains(t, err, "invalid magic")

	badVersionBogusChecksum := bytes.Clone(encoded)
	binary.BigEndian.PutUint16(
		badVersionBogusChecksum[len(taprootAssetTransferMagic):], 9,
	)
	badVersionBogusChecksum[len(badVersionBogusChecksum)-1] ^= 0xff
	err = decoded.UnmarshalBinary(badVersionBogusChecksum)
	require.ErrorIs(t, err, ErrTaprootAssetTransferVersion)
}
