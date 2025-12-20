package psbtutil

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
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
