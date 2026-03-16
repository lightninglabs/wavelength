package arktx

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestCanonicalizeOrderingSortsAndValidates asserts CanonicalizeOrdering
// produces a transaction that passes ValidateCanonicalTx, even if the input tx
// is not canonical.
func TestCanonicalizeOrderingSortsAndValidates(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  [32]byte{2},
			Index: 1,
		},
	})
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  [32]byte{1},
			Index: 0,
		},
	})

	// Add outputs in non-canonical order and with anchor not last.
	tx.TxOut = append(tx.TxOut,
		&wire.TxOut{
			Value:    1,
			PkScript: []byte{0x52},
		},
		arkscript.AnchorOutput(),
		&wire.TxOut{
			Value:    2,
			PkScript: []byte{0x51},
		},
	)

	err := CanonicalizeOrdering(tx)
	require.NoError(t, err)

	err = ValidateCanonicalTx(tx)
	require.NoError(t, err)

	require.True(t, IsAnchorOutput(tx.TxOut[len(tx.TxOut)-1]))
}
