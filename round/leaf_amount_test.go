package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
)

// TestLeafNonAnchorAmountHappyPath verifies the helper returns the
// value of the non-anchor output for a standard leaf tx (VTXO
// output + P2A anchor). This is the single source of truth for
// VTXO amounts under the #270 handshake: the server stamps the
// seal-time residual onto the VTXODescriptor before building the
// tree, so the leaf's non-anchor output carries the quoted value
// rather than the intent target (which is zero for change
// outputs). Reading req.Amount instead would persist stale data
// on the client.
func TestLeafNonAnchorAmountHappyPath(t *testing.T) {
	t.Parallel()

	// A leaf tx has two outputs: the VTXO (or connector) and the
	// P2A anchor. The VTXO output carries the on-chain value.
	const vtxoSat = int64(99_745)

	leaf := &tree.Node{
		Outputs: []*wire.TxOut{
			{
				PkScript: []byte{
					0x51,
					0x20,
					0xaa,
					0xbb,
				},
				Value: vtxoSat,
			},
			arkscript.AnchorOutput(),
		},
	}

	amount, err := leafNonAnchorAmount(leaf)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(vtxoSat), amount)
}

// TestLeafNonAnchorAmountAnchorFirst guards against a position
// assumption: the helper must find the non-anchor output regardless
// of whether the anchor is at index 0 or 1. Tree materializers don't
// guarantee ordering across future changes.
func TestLeafNonAnchorAmountAnchorFirst(t *testing.T) {
	t.Parallel()

	const vtxoSat = int64(42_000)

	leaf := &tree.Node{
		Outputs: []*wire.TxOut{
			arkscript.AnchorOutput(),
			{
				PkScript: []byte{
					0x51,
					0x20,
					0xcc,
					0xdd,
				},
				Value: vtxoSat,
			},
		},
	}

	amount, err := leafNonAnchorAmount(leaf)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(vtxoSat), amount)
}

// TestLeafNonAnchorAmountNilSafe confirms the helper surfaces a
// clean error rather than panicking on a nil leaf. Defense against
// harness / test-scaffold misuse where a tree walker might hand
// this helper a nil node.
func TestLeafNonAnchorAmountNilSafe(t *testing.T) {
	t.Parallel()

	amount, err := leafNonAnchorAmount(nil)
	require.Error(t, err)
	require.Zero(t, amount)
	require.Contains(t, err.Error(), "nil leaf")
}

// TestLeafNonAnchorAmountAllAnchors is a regression guard: if a
// leaf somehow only carries anchor outputs (malformed tree), the
// helper must return an error rather than silently returning zero.
// A silent zero would propagate into ClientVTXO.Amount and corrupt
// the client's balance accounting.
func TestLeafNonAnchorAmountAllAnchors(t *testing.T) {
	t.Parallel()

	leaf := &tree.Node{
		Outputs: []*wire.TxOut{
			arkscript.AnchorOutput(),
			arkscript.AnchorOutput(),
		},
	}

	amount, err := leafNonAnchorAmount(leaf)
	require.Error(t, err)
	require.Zero(t, amount)
	require.Contains(t, err.Error(), "no non-anchor output found")
}

// TestLeafNonAnchorAmountTinyValue verifies the helper preserves
// small amounts correctly (dust-adjacent residuals should round
// trip without any implicit minimum). The seal-time quote builder
// has its own dust check; the amount extractor must not
// double-enforce or silently zero out small values.
func TestLeafNonAnchorAmountTinyValue(t *testing.T) {
	t.Parallel()

	const dustAdjacent = int64(331) // connector-dust floor + 1

	leaf := &tree.Node{
		Outputs: []*wire.TxOut{
			{
				PkScript: []byte{
					0x51,
					0x20,
				},
				Value: dustAdjacent,
			},
			arkscript.AnchorOutput(),
		},
	}

	amount, err := leafNonAnchorAmount(leaf)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(dustAdjacent), amount)
}
