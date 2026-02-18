package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/stretchr/testify/require"
)

// TestParseLockOwnerRoundUsesUUIDBytes verifies we persist round owners as
// canonical UUID bytes rather than textual UUID strings.
func TestParseLockOwnerRoundUsesUUIDBytes(t *testing.T) {
	t.Parallel()

	roundID := uuid.New()
	owner := vtxo.RoundLockOwner(roundID.String())

	kind, ownerID, err := parseLockOwner(owner)
	require.NoError(t, err)
	require.Equal(t, lockOwnerKindRound, kind)
	require.Equal(t, roundID[:], ownerID)
}

// TestParseLockOwnerRoundInvalidUUID ensures malformed round UUID values are
// rejected with an invalid round owner error.
func TestParseLockOwnerRoundInvalidUUID(t *testing.T) {
	t.Parallel()

	_, _, err := parseLockOwner(vtxo.RoundLockOwner("not-a-uuid"))
	require.ErrorContains(t, err, "invalid round owner")
}

// TestRoundLockOwnerRoundTrip checks round owners preserve their original
// canonical value through parse and serialization.
func TestRoundLockOwnerRoundTrip(t *testing.T) {
	t.Parallel()

	roundID := uuid.New()
	owner := vtxo.RoundLockOwner(roundID.String())

	kind, ownerID, err := parseLockOwner(owner)
	require.NoError(t, err)

	roundTrip := lockOwnerToValue(kind, ownerID)
	require.Equal(t, owner, roundTrip)
}
