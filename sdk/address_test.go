package sdk

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecodeReceiveAddress(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	addr, err := encodeReceiveAddress(
		"tark", operatorPriv.PubKey(), recipientPriv.PubKey(), 144,
	)
	require.NoError(t, err)
	require.NotEmpty(t, addr)

	decoded, err := decodeReceiveAddress(addr)
	require.NoError(t, err)
	require.Equal(t, "tark", decoded.hrp)
	require.Equal(t, uint32(144), decoded.exitDelay)
	require.Equal(t, schnorr.SerializePubKey(operatorPriv.PubKey()),
		schnorr.SerializePubKey(decoded.operatorKey))
	require.Equal(t, schnorr.SerializePubKey(recipientPriv.PubKey()),
		schnorr.SerializePubKey(decoded.recipientKey))
}

func TestDecodeReceiveAddressRejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	addr, err := encodeReceiveAddress(
		"tark", operatorPriv.PubKey(), recipientPriv.PubKey(), 144,
	)
	require.NoError(t, err)

	hrp, addrData, err := bech32.DecodeNoLimit(addr)
	require.NoError(t, err)

	payload, err := bech32.ConvertBits(addrData, 5, 8, false)
	require.NoError(t, err)
	payload[0] = receiveAddressVersionV1 + 1

	modifiedData, err := bech32.ConvertBits(payload, 8, 5, true)
	require.NoError(t, err)

	modifiedAddr, err := bech32.EncodeM(hrp, modifiedData)
	require.NoError(t, err)

	_, err = decodeReceiveAddress(modifiedAddr)
	require.ErrorContains(t, err, "unsupported recipient address version")
}
