package batchcanon

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStateValuesStable pins the integer value and string name of every
// canonicality state. These values are persisted as a typed INTEGER column,
// so a change here would silently re-interpret existing rows — the test
// exists to make any renumbering a deliberate, visible edit.
func TestStateValuesStable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		state State
		value int
		name  string
	}{
		{
			StateUnseen,
			0,
			"unseen",
		},
		{
			StateProvisional,
			1,
			"provisional",
		},
		{
			StateFinalized,
			2,
			"finalized",
		},
		{
			StateReorgedOut,
			3,
			"reorged_out",
		},
		{
			StateConflictProvisional,
			4,
			"conflict_provisional",
		},
		{
			StateConflictFinalized,
			5,
			"conflict_finalized",
		},
	}

	for _, tc := range cases {
		require.Equal(t, tc.value, int(tc.state), tc.name)
		require.Equal(t, tc.name, tc.state.String())
	}
}

// TestStateStringUnknown verifies an out-of-range state stringifies to a
// diagnosable unknown form rather than an empty string.
func TestStateStringUnknown(t *testing.T) {
	t.Parallel()

	require.Equal(t, "unknown(99)", State(99).String())
}

// TestPolicyStateStable pins the policy-state value and name. PolicyState is
// also persisted as an append-only typed INTEGER column.
func TestPolicyStateStable(t *testing.T) {
	t.Parallel()

	require.Equal(t, 0, int(PolicyStateDefault))
	require.Equal(t, "default", PolicyStateDefault.String())
	require.Equal(t, "unknown(7)", PolicyState(7).String())
}
