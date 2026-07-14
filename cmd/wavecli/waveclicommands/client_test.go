package waveclicommands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWavecliUsesDaemonDefaultAuthPaths(t *testing.T) {
	t.Parallel()

	cmd := NewRootCmd()
	require.NoError(t, cmd.PersistentFlags().Set("datadir", "/tmp/waved"))
	require.NoError(t, cmd.PersistentFlags().Set("network", "regtest"))

	tlsCertPath, err := daemonTLSCertPath(cmd)
	require.NoError(t, err)
	require.Equal(
		t, filepath.Join(
			"/tmp/waved", "data", "regtest", "tls.cert",
		),
		tlsCertPath,
	)

	macaroonPath, err := daemonMacaroonPath(cmd)
	require.NoError(t, err)
	require.Equal(
		t, filepath.Join(
			"/tmp/waved", "data", "regtest", "admin.macaroon",
		),
		macaroonPath,
	)
}

func TestWavecliExpandsDefaultTildeDataDir(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	cmd := NewRootCmd()
	require.NoError(t, cmd.PersistentFlags().Set("datadir", "~/.waved"))
	require.NoError(t, cmd.PersistentFlags().Set("network", "mainnet"))

	tlsCertPath, err := daemonTLSCertPath(cmd)
	require.NoError(t, err)
	require.Equal(
		t, filepath.Join(
			home, ".waved", "data", "mainnet", "tls.cert",
		),
		tlsCertPath,
	)
}

func TestWavecliAuthPathOverrides(t *testing.T) {
	t.Parallel()

	cmd := NewRootCmd()
	require.NoError(
		t, cmd.PersistentFlags().Set(
			"tlscertpath", "/tmp/tls.cert",
		),
	)
	require.NoError(
		t, cmd.PersistentFlags().Set(
			"macaroonpath", "/tmp/admin.macaroon",
		),
	)

	tlsCertPath, err := daemonTLSCertPath(cmd)
	require.NoError(t, err)
	require.Equal(t, "/tmp/tls.cert", tlsCertPath)

	macaroonPath, err := daemonMacaroonPath(cmd)
	require.NoError(t, err)
	require.Equal(t, "/tmp/admin.macaroon", macaroonPath)
}
