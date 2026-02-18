package conn

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStableEventIDs verifies deterministic identity derivation from payload.
func TestStableEventIDs(t *testing.T) {
	t.Parallel()

	payload := []byte("same-payload")

	msgID1 := StableEventMsgID(payload)
	msgID2 := StableEventMsgID(payload)
	require.Equal(t, msgID1, msgID2)
	require.Contains(t, msgID1, "evt-")

	idem1 := StableEventIdempotencyKey(payload)
	idem2 := StableEventIdempotencyKey(payload)
	require.Equal(t, idem1, idem2)
	require.Contains(t, idem1, "idem-")
}
