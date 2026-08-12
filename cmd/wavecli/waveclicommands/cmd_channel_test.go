package waveclicommands

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseChannelIDAcceptsCLIEncodings verifies command chaining works with
// both the protobuf JSON response and the canonical hexadecimal identifier.
func TestParseChannelIDAcceptsCLIEncodings(t *testing.T) {
	t.Parallel()

	id := make([]byte, 32)
	for i := range id {
		id[i] = byte(i + 1)
	}

	for _, encoded := range []string{
		hex.EncodeToString(id), base64.StdEncoding.EncodeToString(id),
	} {
		decoded, err := parseChannelID(encoded)
		require.NoError(t, err)
		require.Equal(t, id, decoded)
	}
}

// TestParseChannelIDRejectsWrongLength verifies malformed identifiers fail
// before the CLI opens a daemon connection.
func TestParseChannelIDRejectsWrongLength(t *testing.T) {
	t.Parallel()

	_, err := parseChannelID(hex.EncodeToString(make([]byte, 31)))
	require.ErrorContains(t, err, "exactly 32 bytes")
}

// TestParsePositiveChannelAmount keeps channel creation's public input to one
// positive amount instead of exposing internal funding-policy switches.
func TestParsePositiveChannelAmount(t *testing.T) {
	t.Parallel()

	amount, err := parsePositiveChannelAmount("200000")
	require.NoError(t, err)
	require.EqualValues(t, 200_000, amount)

	for _, value := range []string{"0", "-1", "one"} {
		_, err := parsePositiveChannelAmount(value)
		require.ErrorContains(t, err, "positive integer")
	}
}
