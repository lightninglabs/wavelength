package round

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestRoundIDBytesValidUUID verifies that a canonical RoundID
// string round-trips to its 16-byte form intact. uuid.Parse is
// the authoritative parser; the test pins the behaviour so a
// future switch to a different form factor (hex, etc.) does not
// silently change what gets stored in the ledger.
func TestRoundIDBytesValidUUID(t *testing.T) {
	t.Parallel()

	id := uuid.New()

	got := roundIDBytes(id.String())
	require.Equal(t, [16]byte(id[:]), got)
}

// TestRoundIDBytesInvalidReturnsZero covers the fallback path
// where a malformed RoundID string decays to the zero array.
// The ledger handler stores zero as NULL via roundIDOrNil so a
// bad input degrades to a non-round-tagged entry rather than
// rejecting the whole message.
func TestRoundIDBytesInvalidReturnsZero(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"not-a-uuid",
		"abcdef",
		"12345678-1234-1234-1234-1234567890ZZ", // bad hex char
	}

	var zero [16]byte
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			require.Equal(t, zero, roundIDBytes(in))
		})
	}
}
