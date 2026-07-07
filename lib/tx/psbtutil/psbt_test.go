package psbtutil

import (
	"encoding/base64"
	"testing"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestSerializeParseRoundTrip asserts PSBT serialization is reversible.
func TestSerializeParseRoundTrip(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{})
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})

	pkt, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	raw, err := Serialize(pkt)
	require.NoError(t, err)

	parsed, err := Parse(raw)
	require.NoError(t, err)
	require.NotNil(t, parsed)
	require.Equal(t, tx.TxHash(), parsed.UnsignedTx.TxHash())
}

// TestEncodeDecodeBase64RoundTrip asserts base64 encode/decode is reversible.
func TestEncodeDecodeBase64RoundTrip(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{})
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})

	pkt, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	b64, err := EncodeBase64(pkt)
	require.NoError(t, err)
	require.NotEmpty(t, b64)

	decoded, err := DecodeBase64(b64)
	require.NoError(t, err)
	require.NotNil(t, decoded)
	require.Equal(t, tx.TxHash(), decoded.UnsignedTx.TxHash())
}

// TestEncodeBase64NilPSBT asserts nil PSBTs are rejected.
func TestEncodeBase64NilPSBT(t *testing.T) {
	t.Parallel()

	_, err := EncodeBase64(nil)
	require.Error(t, err)
}

// TestDecodeBase64EmptyString asserts empty base64 strings are rejected.
func TestDecodeBase64EmptyString(t *testing.T) {
	t.Parallel()

	_, err := DecodeBase64("")
	require.Error(t, err)
}

// TestDecodeBase64InvalidEncoding asserts invalid base64 is rejected.
func TestDecodeBase64InvalidEncoding(t *testing.T) {
	t.Parallel()

	invalid := base64.StdEncoding.EncodeToString([]byte{0x00, 0x01})
	invalid = invalid[:len(invalid)-2]

	_, err := DecodeBase64(invalid)
	require.Error(t, err)
}
