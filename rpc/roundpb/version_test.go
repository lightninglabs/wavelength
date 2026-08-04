package roundpb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateFlowVersion proves the round flow-version guard: every version
// up to the latest this build understands passes; any higher, unknown value
// is rejected so a round conducted under rules this build does not understand
// is never acted upon.
func TestValidateFlowVersion(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateFlowVersion(FlowVersionV1))
	require.NoError(t, ValidateFlowVersion(FlowVersionV2))

	require.Error(t, ValidateFlowVersion(latestFlowVersion+1))
	require.Error(t, ValidateFlowVersion(99))
}
