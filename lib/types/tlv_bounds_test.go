package types

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSafeTypesTLVBytesCap checks that the message cap admits a join-auth
// message larger than 16 MiB, which the per-list limits allow, and rejects
// one beyond anything those limits can produce.
func TestSafeTypesTLVBytesCap(t *testing.T) {
	t.Parallel()

	// A run of zero bytes is a well-framed stream of empty type-0 records,
	// so only the size gate decides the outcome.
	_, err := safeTypesTLVBytes(make([]byte, 17<<20))
	require.NoError(t, err)

	_, err = safeTypesTLVBytes(make([]byte, maxTypesMessageSize+1))
	require.ErrorIs(t, err, ErrInvalidTLV)
}
