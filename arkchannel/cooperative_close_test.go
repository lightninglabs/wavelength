package arkchannel

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// mustDecodeTransaction decodes a raw transaction fixture.
func mustDecodeTransaction(t *testing.T, raw []byte) *wire.MsgTx {
	t.Helper()
	tx := wire.NewMsgTx(2)
	require.NoError(t, tx.Deserialize(bytes.NewReader(raw)))

	return tx
}
