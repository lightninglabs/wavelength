package treeconv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTreePathProtoNil verifies the compatibility wrapper preserves nil
// handling from the canonical arkrpc converter.
func TestTreePathProtoNil(t *testing.T) {
	t.Parallel()

	pb, err := TreePathFromTree(nil)
	require.NoError(t, err)
	require.Nil(t, pb)

	got, err := TreePathToTree(nil)
	require.NoError(t, err)
	require.Nil(t, got)
}
