package actormsg

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestExitPolicyKindValid verifies every durable non-standard exit policy can
// pass the VTXO manager's recovery-only admission guard.
func TestExitPolicyKindValid(t *testing.T) {
	t.Parallel()

	valid := []ExitPolicyKind{
		ExitPolicyVHTLCClaim,
		ExitPolicyVHTLCRefundWithoutReceiver,
		ExitPolicyArkChannelBacking,
	}
	for _, kind := range valid {
		require.True(t, kind.Valid(), kind)
	}

	require.False(t, ExitPolicyKind("unknown").Valid())
}
