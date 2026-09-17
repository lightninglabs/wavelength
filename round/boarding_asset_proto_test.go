package round

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestAssetBoardingProtoRoundTrip preserves the disclosure when captured
// mailbox requests are decoded, without sharing mutable bytes with the wire.
func TestAssetBoardingProtoRoundTrip(t *testing.T) {
	t.Parallel()

	_, owner := btcec.PrivKeyFromBytes([]byte{1})
	_, operator := btcec.PrivKeyFromBytes([]byte{2})
	policy, err := arkscript.EncodeStandardVTXOTemplate(
		owner, operator, 144,
	)
	require.NoError(t, err)

	request := &JoinRoundRequest{
		BoardingRequests: []types.BoardingRequest{{
			Outpoint: &wire.OutPoint{
				Index: 1,
			},
			PolicyTemplate: policy,
			AssetRef: "assetid:" +
				"01010101010101010101010101010101" +
				"01010101010101010101010101010101",
			AssetAmount: 42,
			AssetDigest: bytes.Repeat([]byte{2}, 32),
			AssetProof:  []byte("confirmed proof"),
			AssetCommitmentLeafHash: bytes.Repeat(
				[]byte{3}, 32,
			),
			AssetWitness: [][]byte{
				{
					0x51,
				},
				{
					0xc0,
					4,
				},
			},
		}},
	}
	encoded := request.ToProto().UnwrapOrFail(t)
	snapshot := proto.Clone(encoded)
	var decoded JoinRoundRequest
	require.NoError(t, decoded.FromProto(encoded))
	roundTrip := decoded.ToProto().UnwrapOrFail(t)
	require.True(t, proto.Equal(encoded, roundTrip))

	disclosure := &decoded.BoardingRequests[0]
	disclosure.AssetDigest[0]++
	disclosure.AssetProof[0]++
	disclosure.AssetCommitmentLeafHash[0]++
	disclosure.AssetWitness[0][0]++
	disclosure.AssetWitness[1] = []byte{5}
	require.True(t, proto.Equal(snapshot, encoded))
}
