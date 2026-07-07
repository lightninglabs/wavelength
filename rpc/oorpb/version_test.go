package oorpb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateFlowVersion proves the OOR flow-version guard: V1 (the
// zero-indexed value 0) passes; any higher, unknown value is rejected so a
// transfer conducted under rules this build does not understand is never
// resumed.
func TestValidateFlowVersion(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateFlowVersion(FlowVersionV1))

	require.Error(t, ValidateFlowVersion(FlowVersionV1+1))
	require.Error(t, ValidateFlowVersion(99))
}
