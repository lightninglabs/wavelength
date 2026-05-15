//go:build systest

package systest

import (
	"testing"

	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/stretchr/testify/require"
)

// TestFindPendingTimeoutIDMatchesPhaseSuffix asserts that manual timeout
// helpers match the phase suffix while preserving the complete timeout ID.
func TestFindPendingTimeoutIDMatchesPhaseSuffix(t *testing.T) {
	t.Parallel()

	const wantID = timeout.ID("round-id:registration")

	id, ok := findPendingTimeoutID(
		rounds.TimeoutPhaseRegistration,
		[]timeout.ID{
			"round-id:vtxo_nonces",
			wantID,
		},
	)
	require.True(t, ok)
	require.Equal(t, wantID, id)
}

// TestFindPendingTimeoutIDRejectsPhaseSubstring asserts that timeout matching
// only accepts the phase suffix and not unrelated substring matches.
func TestFindPendingTimeoutIDRejectsPhaseSubstring(t *testing.T) {
	t.Parallel()

	_, ok := findPendingTimeoutID(
		rounds.TimeoutPhaseRegistration,
		[]timeout.ID{
			"registration:round-id",
			"round-id:registration-extra",
		},
	)
	require.False(t, ok)
}
