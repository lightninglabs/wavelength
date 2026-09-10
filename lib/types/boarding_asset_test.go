package types

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAssetBoardingAuth binds the deposit claim while proof bytes and the
// OP_TRUE witness remain independently authenticated by the asset verifier.
func TestAssetBoardingAuth(t *testing.T) {
	t.Parallel()
	req := testJoinRoundAuthRequest(t)
	bitcoin, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	board := req.BoardingReqs[0]
	board.AssetRef = "assetid:asset"
	board.AssetAmount = 900
	board.AssetDigest = bytes.Repeat([]byte{1}, 32)
	board.AssetCommitmentLeafHash = bytes.Repeat([]byte{2}, 32)
	original, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	require.NotEqual(t, bitcoin, original)
	decoded, err := DecodeJoinRoundAuthMessage(original)
	require.NoError(t, err)
	require.Equal(t, board.AssetRef, decoded.BoardingReqs[0].AssetRef)
	require.Equal(t, board.AssetAmount, decoded.BoardingReqs[0].AssetAmount)
	require.Equal(t, board.AssetDigest, decoded.BoardingReqs[0].AssetDigest)
	require.Equal(
		t, board.AssetCommitmentLeafHash,
		decoded.BoardingReqs[0].AssetCommitmentLeafHash,
	)
	for _, mutate := range []func(*BoardingRequest){
		func(r *BoardingRequest) {
			r.AssetRef += "changed"
		},
		func(r *BoardingRequest) {
			r.AssetAmount++
		},
		func(r *BoardingRequest) {
			r.AssetDigest = bytes.Repeat([]byte{3}, 32)
		},
		func(r *BoardingRequest) {
			r.AssetCommitmentLeafHash = bytes.Repeat([]byte{4}, 32)
		},
	} {
		changed := *board
		mutate(&changed)
		req.BoardingReqs[0] = &changed
		encoded, err := JoinRoundAuthMessage(req)
		require.NoError(t, err)
		require.NotEqual(t, original, encoded)
	}
	req.BoardingReqs[0] = board
	board.AssetProof = []byte("proof")
	board.AssetWitness = [][]byte{{0x51}, {1}}
	encoded, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	require.Equal(t, original, encoded)
}
