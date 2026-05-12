package arkscript

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAnchorPkScriptPinned pins the anchor output's pkScript bytes
// to the exact 4-byte P2A sequence. The round FSM's
// leafNonAnchorAmount distinguishes anchor outputs from VTXO
// outputs by byte-equality against this constant; any silent drift
// here would misclassify leaf outputs and either leak anchor
// values into persisted VTXOs or drop real VTXOs as anchors. A
// deliberate change must update this test alongside the constant.
func TestAnchorPkScriptPinned(t *testing.T) {
	t.Parallel()

	expected := []byte{0x51, 0x02, 0x4e, 0x73}
	require.Equal(t, expected, AnchorPkScript)

	// AnchorOutput returns a defensive copy but must still carry
	// the pinned script verbatim.
	got := AnchorOutput()
	require.Equal(t, expected, got.PkScript)
	require.Equal(t, int64(0), got.Value)

	// Mutating the returned script must not affect subsequent
	// callers (defensive-copy invariant).
	got.PkScript[0] = 0x00
	require.Equal(
		t, expected, AnchorPkScript,
		"AnchorOutput must return a defensive copy",
	)
	require.Equal(
		t, expected, AnchorOutput().PkScript,
		"subsequent AnchorOutput must still match",
	)
}
