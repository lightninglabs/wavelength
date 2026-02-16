package oor

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

func TestStartTransferPayloadTLVRoundTrip(t *testing.T) {
	t.Parallel()

	payload := startTransferPayload{
		OperatorPubKey: []byte{2, 1, 2, 3},
		CSVDelay:       144,
		Inputs: []*TransferInputSnapshot{
			{
				Outpoint: wire.OutPoint{
					Hash:  chainhash.Hash{1, 2, 3},
					Index: 7,
				},
				AmountSat:       1000,
				ClientKeyFamily: 1,
				ClientKeyIndex:  9,
				ClientPubKey:    []byte{2, 4, 6},
				OperatorPubKey:  []byte{3, 5, 7},
				ExitDelay:       72,
				OwnerLeafScript: []byte{0x51},
			},
		},
		Recipients: []recipientPayload{
			{
				PkScript: []byte{0x51, 0x20},
				ValueSat: 321,
			},
		},
	}

	raw, err := encodeStartTransferPayload(payload)
	require.NoError(t, err)

	decoded, err := decodeStartTransferPayload(raw)
	require.NoError(t, err)

	require.Equal(t, payload.OperatorPubKey, decoded.OperatorPubKey)
	require.Equal(t, payload.CSVDelay, decoded.CSVDelay)
	require.Equal(t, payload.Recipients, decoded.Recipients)
	require.Len(t, decoded.Inputs, 1)
	require.Equal(t, payload.Inputs[0], decoded.Inputs[0])
}

func TestSessionPayloadTLVRoundTrip(t *testing.T) {
	t.Parallel()

	id := SessionID(chainhash.Hash{9, 8, 7, 6})
	raw, err := encodeSessionPayload(id)
	require.NoError(t, err)

	decoded, err := decodeSessionPayload(raw)
	require.NoError(t, err)
	require.Equal(t, id, decoded)
}

func TestDecodeLengthPrefixedBlobListRejectsTrailingBytes(t *testing.T) {
	t.Parallel()

	raw, err := encodeLengthPrefixedBlobList(
		[][]byte{{1, 2, 3}},
	)
	require.NoError(t, err)

	raw = append(raw, 0xff)
	_, err = decodeLengthPrefixedBlobList(raw)
	require.ErrorContains(t, err, "trailing payload bytes")
}
