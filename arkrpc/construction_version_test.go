package arkrpc

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateConstructionVersion proves the construction-version guard: V1
// (the zero-indexed value 0) passes; any higher, unknown value is rejected so
// an object built under rules this build does not understand is never acted
// upon.
func TestValidateConstructionVersion(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateConstructionVersion(ConstructionVersionV1))

	require.Error(t, ValidateConstructionVersion(ConstructionVersionV1+1))
	require.Error(t, ValidateConstructionVersion(99))
}
