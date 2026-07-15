package tapassets

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFileStorePersistsAndReplaces proves the PoC journal survives a new
// store instance and atomically exposes only the latest request state.
func TestFileStorePersistsAndReplaces(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "journal")
	store, err := NewFileStore(dir)
	require.NoError(t, err)
	_, err = store.Load(t.Context(), "request")
	require.ErrorIs(t, err, ErrStoreNotFound)

	require.NoError(
		t,
		store.Store(
			t.Context(),
			"request", []byte("attempt"),
		),
	)
	restarted, err := NewFileStore(dir)
	require.NoError(t, err)
	value, err := restarted.Load(t.Context(), "request")
	require.NoError(t, err)
	require.Equal(t, []byte("attempt"), value)

	require.NoError(
		t,
		restarted.Store(
			t.Context(),
			"request", []byte("sealed"),
		),
	)
	value, err = store.Load(t.Context(), "request")
	require.NoError(t, err)
	require.Equal(t, []byte("sealed"), value)

	info, err := os.Stat(store.path("request"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestFileStoreContainsUntrustedRequestIDs proves request identities cannot
// select paths outside the configured journal directory.
func TestFileStoreContainsUntrustedRequestIDs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := NewFileStore(dir)
	require.NoError(t, err)
	require.Equal(t, dir, filepath.Dir(store.path("../../escape")))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = store.Store(ctx, "request", []byte("state"))
	require.ErrorIs(t, err, context.Canceled)
}
