package waved

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConfigLogDirUsesNetworkScopedDefault verifies the default log directory
// stays under the configured data directory and current network.
func TestConfigLogDirUsesNetworkScopedDefault(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dataDir
	cfg.Network = "regtest"

	require.Equal(
		t, filepath.Join(dataDir, "logs", "regtest"), cfg.LogDir(),
	)
}

// TestConfigLogDirUsesExplicitLogDir verifies an explicit log directory
// replaces the default data-directory-derived path.
func TestConfigLogDirUsesExplicitLogDir(t *testing.T) {
	t.Parallel()

	explicitLogDir := filepath.Join(t.TempDir(), "custom-logs")
	cfg := DefaultConfig()
	cfg.DataDir = filepath.Join(t.TempDir(), "unused-data")
	cfg.Network = "regtest"
	cfg.LogDirPath = explicitLogDir

	require.Equal(t, explicitLogDir, cfg.LogDir())
}
