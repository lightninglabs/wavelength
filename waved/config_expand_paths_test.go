package waved

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateExpandsTildePaths verifies Validate normalizes the leading
// tilde on filesystem path fields so downstream consumers can read them
// as absolute strings without repeating the expansion.
func TestValidateExpandsTildePaths(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	cfg := DefaultConfig()
	cfg.Network = "regtest"
	cfg.DataDir = "~/.waved"
	cfg.LogDirPath = "~/.waved/logs-override"
	cfg.Wallet.EsploraURL = "https://esplora.example/api"
	cfg.Lnd.TLSPath = "~/.lnd/tls.cert"
	cfg.Lnd.MacaroonPath = "~/.lnd/admin.macaroon"
	cfg.Server.TLSCertPath = "~/ark/tls.cert"
	cfg.RPC.TLSCertPath = "~/.waved/rpc-tls.cert"
	cfg.RPC.TLSKeyPath = "~/.waved/rpc-tls.key"
	cfg.Wallet.PasswordFile = "~/secrets/pw"
	cfg.Wallet.BtcwalletDataDir = "~/.waved/neutrino"
	cfg.Swap.DatabaseFileName = "~/.waved/custom-swaps.db"
	cfg.Swap.ServerTLSCertPath = "~/.waved/swap-tls.cert"

	require.NoError(t, cfg.Validate())

	require.Equal(t, filepath.Join(home, ".waved"), cfg.DataDir)
	require.Equal(
		t, filepath.Join(home, ".waved", "logs-override"),
		cfg.LogDirPath,
	)
	require.Equal(
		t, filepath.Join(home, ".lnd", "tls.cert"), cfg.Lnd.TLSPath,
	)
	require.Equal(
		t, filepath.Join(home, ".lnd", "admin.macaroon"),
		cfg.Lnd.MacaroonPath,
	)
	require.Equal(
		t, filepath.Join(home, "ark", "tls.cert"),
		cfg.Server.TLSCertPath,
	)
	require.Equal(
		t, filepath.Join(home, ".waved", "rpc-tls.cert"),
		cfg.RPC.TLSCertPath,
	)
	require.Equal(
		t, filepath.Join(home, ".waved", "rpc-tls.key"),
		cfg.RPC.TLSKeyPath,
	)
	require.Equal(
		t, filepath.Join(home, "secrets", "pw"),
		cfg.Wallet.PasswordFile,
	)
	require.Equal(
		t, filepath.Join(home, ".waved", "neutrino"),
		cfg.Wallet.BtcwalletDataDir,
	)
	require.Equal(
		t, filepath.Join(home, ".waved", "custom-swaps.db"),
		cfg.Swap.DatabaseFileName,
	)
	require.Equal(
		t, filepath.Join(home, ".waved", "swap-tls.cert"),
		cfg.Swap.ServerTLSCertPath,
	)

	// NetworkDir and LogDir read the already-expanded values without
	// re-expanding, so the result is a clean absolute path.
	require.Equal(
		t, filepath.Join(home, ".waved", "data", "regtest"),
		cfg.NetworkDir(),
	)
	require.Equal(
		t, filepath.Join(home, ".waved", "logs-override"), cfg.LogDir(),
	)
}

// TestValidateLeavesAbsolutePathsUntouched verifies the expansion step is a
// no-op for callers that already pass absolute paths.
func TestValidateLeavesAbsolutePathsUntouched(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Network = "regtest"
	cfg.DataDir = dataDir
	cfg.Wallet.EsploraURL = "https://esplora.example/api"

	require.NoError(t, cfg.Validate())
	require.Equal(t, dataDir, cfg.DataDir)
}
