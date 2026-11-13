package swaprecovery

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/stretchr/testify/require"
)

// TestPreimageVector pins the public encoding and HKDF output against an
// independently calculated HMAC-SHA256 extract/expand reference vector.
func TestPreimageVector(t *testing.T) {
	t.Parallel()

	params := make([]byte, ParamsSize)
	params[0] = Version
	binary.BigEndian.PutUint32(params[1:5], 7)
	copy(params[5:], bytes.Repeat([]byte{2}, 32))
	preimage, err := Derive(
		params, *chaincfg.MainNetParams.GenesisHash, [32]byte{1},
	)
	require.NoError(t, err)
	require.Equal(
		t, "bfc90019ba074932fe8cf466c9267daae415949a3143dc1e81783f6e"+
			"933d8c58", hex.EncodeToString(preimage[:]),
	)
}

// TestPreimageDerivation checks determinism and separation across nonce,
// locator, wallet secret, and network. These are the restore safety boundaries.
func TestPreimageDerivation(t *testing.T) {
	t.Parallel()

	params, err := NewParams(7)
	require.NoError(t, err)
	loc, err := ParseParams(params)
	require.NoError(t, err)
	require.Equal(t, KeyFamily, loc.Family)
	require.EqualValues(t, 46, loc.Family)
	require.EqualValues(t, 7, loc.Index)
	genesis := *chaincfg.RegressionNetParams.GenesisHash
	secret := [32]byte{1}
	first, err := Derive(params, genesis, secret)
	require.NoError(t, err)
	again, err := Derive(params, genesis, secret)
	require.NoError(t, err)
	require.Equal(t, first, again)

	newNonce, err := NewParams(7)
	require.NoError(t, err)
	newIndex := append([]byte(nil), params...)
	binary.BigEndian.PutUint32(newIndex[1:5], 8)
	for _, other := range [][]byte{newNonce, newIndex} {
		derived, err := Derive(other, genesis, secret)
		require.NoError(t, err)
		require.NotEqual(t, first, derived)
	}
	otherSeed, err := Derive(params, genesis, [32]byte{2})
	require.NoError(t, err)
	require.NotEqual(t, first, otherSeed)
	otherNetwork, err := Derive(
		params, *chaincfg.MainNetParams.GenesisHash, secret,
	)
	require.NoError(t, err)
	require.NotEqual(t, first, otherNetwork)
}

// TestParseParamsRejectsInvalid prevents malformed public metadata from
// selecting unsupported key indices or derivation versions.
func TestParseParamsRejectsInvalid(t *testing.T) {
	t.Parallel()

	valid, err := NewParams(0)
	require.NoError(t, err)
	unknown := append([]byte(nil), valid...)
	unknown[0] = 2
	indexOverflow := append([]byte(nil), valid...)
	binary.BigEndian.PutUint32(indexOverflow[1:5], 1<<31)
	for _, params := range [][]byte{
		nil, valid[:1], append(valid, 0), unknown, indexOverflow,
	} {
		_, err := ParseParams(params)
		require.Error(t, err)
	}
	_, err = NewParams(1 << 31)
	require.Error(t, err)
}
