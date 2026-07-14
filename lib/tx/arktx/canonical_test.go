package arktx

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
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

// TestAnchorPredicates asserts the relationship between the zero-value
// ephemeral anchor predicate (IsAnchorOutput), the funded anchor predicate
// (IsFundedAnchorOutput), and the value-agnostic script matcher
// (IsP2AAnchorScript). A funded anchor must be recognised as a P2A anchor by
// script while being excluded from the strict zero-value ephemeral form.
func TestAnchorPredicates(t *testing.T) {
	t.Parallel()

	ephemeral := arkscript.AnchorOutput()
	funded := arkscript.AnchorOutput(arkscript.WithAnchorValue(330))
	nonAnchor := &wire.TxOut{Value: 330, PkScript: []byte{0x51, 0x20}}

	// The script matcher accepts both anchor forms and rejects everything
	// else, since it ignores value entirely.
	require.True(t, IsP2AAnchorScript(ephemeral.PkScript))
	require.True(t, IsP2AAnchorScript(funded.PkScript))
	require.False(t, IsP2AAnchorScript(nonAnchor.PkScript))

	// The strict ephemeral predicate accepts only the zero-value form.
	require.True(t, IsAnchorOutput(ephemeral))
	require.False(t, IsAnchorOutput(funded))
	require.False(t, IsAnchorOutput(nonAnchor))

	// The funded predicate is the exact complement over P2A outputs: it
	// accepts the non-zero anchor and rejects the ephemeral one.
	require.False(t, IsFundedAnchorOutput(ephemeral))
	require.True(t, IsFundedAnchorOutput(funded))
	require.False(t, IsFundedAnchorOutput(nonAnchor))
	require.False(t, IsFundedAnchorOutput(nil))
}
