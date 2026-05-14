package darepod

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

	logDir, err := cfg.LogDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dataDir, "logs", "regtest"), logDir)
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

	logDir, err := cfg.LogDir()
	require.NoError(t, err)
	require.Equal(t, explicitLogDir, logDir)
}
