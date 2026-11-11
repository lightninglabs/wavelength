package swaps

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestRecoveryListOwnerProof checks the SDK signs every audience/page field
// the server verifies, without consuming the nonce on an identical retry.
func TestRecoveryListOwnerProof(t *testing.T) {
	t.Parallel()

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	server, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	daemon := &testDaemonConn{identityPriv: key}
	genesis := *chaincfg.RegressionNetParams.GenesisHash
	token := append([]byte{1}, bytes.Repeat([]byte{2}, 32)...)
	proof, err := newSwapListOwnerProof(
		t.Context(), daemon, key.PubKey(), server.PubKey(), genesis, 2,
		token, 1700000000,
	)
	require.NoError(t, err)
	sig, err := schnorr.ParseSignature(proof.GetSignature())
	require.NoError(t, err)
	fields := [][]byte{server.PubKey().SerializeCompressed(), genesis[:],
		recoveryUint64Field(2), token}
	message := swapRecoveryAuthMessage(
		swapRecoveryAuthList, proof.GetClientIdentityPubkey(),
		proof.GetTimestampUnix(), proof.GetNonce(), fields...,
	)
	digest := chainhash.TaggedHash([]byte(swapRecoveryAuthTag), message)
	require.True(t, sig.Verify(digest[:], key.PubKey()))
	for i := range fields {
		changed := append([][]byte(nil), fields...)
		changed[i] = append([]byte(nil), fields[i]...)
		changed[i][0] ^= 1
		message = swapRecoveryAuthMessage(
			swapRecoveryAuthList, proof.GetClientIdentityPubkey(),
			proof.GetTimestampUnix(), proof.GetNonce(), changed...,
		)
		digest = chainhash.TaggedHash(
			[]byte(swapRecoveryAuthTag), message,
		)
		require.False(t, sig.Verify(digest[:], key.PubKey()))
	}
	_, err = newSwapListOwnerProof(
		t.Context(), daemon, key.PubKey(), nil, genesis, 2, token,
		1700000000,
	)
	require.ErrorContains(t, err, "server identity is required")
}

// TestRecoveryListSigningVector pins the transcript against an independent
// length-prefix and SHA256 reference implementation, shared with server tests.
func TestRecoveryListSigningVector(t *testing.T) {
	t.Parallel()

	_, owner := btcec.PrivKeyFromBytes([]byte{1})
	_, server := btcec.PrivKeyFromBytes([]byte{2})
	message := swapRecoveryAuthMessage(
		swapRecoveryAuthList, owner.SerializeCompressed(), 1700000000,
		bytes.Repeat(
			[]byte{9}, 16,
		),
		server.SerializeCompressed(),
		bytes.Repeat(
			[]byte{3}, 32,
		),
		recoveryUint64Field(2),
		bytes.Repeat(
			[]byte{1}, 33,
		),
	)
	digest := chainhash.TaggedHash([]byte(swapRecoveryAuthTag), message)
	require.Equal(
		t, "e5c21abeafdc40c72f8d77d536085335c0218963c15e79e97a544829"+
			"c93ac7b5", hex.EncodeToString(digest[:]),
	)
}

type recoveryPageRPCClient struct {
	swaprpc.SwapServiceClient
	request  *swaprpc.ListRecoverableSwapsRequest
	response *swaprpc.ListRecoverableSwapsResponse
}

// ListRecoverableSwaps records the exact wire page for transport assertions.
func (c *recoveryPageRPCClient) ListRecoverableSwaps(_ context.Context,
	req *swaprpc.ListRecoverableSwapsRequest, _ ...grpc.CallOption) (
	*swaprpc.ListRecoverableSwapsResponse, error) {

	c.request = req

	return c.response, nil
}

// TestRecoveryPageTransport prevents normalization or truncation of signed
// page fields and continuation metadata at the SDK transport boundary.
func TestRecoveryPageTransport(t *testing.T) {
	t.Parallel()

	client := &recoveryPageRPCClient{
		response: &swaprpc.ListRecoverableSwapsResponse{
			NextPageToken: []byte{
				1,
				2,
			},
		},
	}
	conn := newSwapServerConn(client)
	proof := &swaprpc.SwapOwnerProof{Signature: []byte{3}}
	token := []byte{4, 5}
	response, err := conn.ListRecoverableSwapsPage(
		t.Context(), proof, 0, token,
	)
	require.NoError(t, err)
	require.Same(t, proof, client.request.GetOwnerProof())
	require.Zero(t, client.request.GetPageSize())
	require.Equal(t, token, client.request.GetPageToken())
	require.Equal(t, []byte{1, 2}, response.GetNextPageToken())
}
