package darepoclicommands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDarepocliUsesDaemonDefaultAuthPaths(t *testing.T) {
	t.Parallel()

	cmd := NewRootCmd()
	require.NoError(t, cmd.PersistentFlags().Set("datadir", "/tmp/darepod"))
	require.NoError(t, cmd.PersistentFlags().Set("network", "regtest"))

	tlsCertPath, err := daemonTLSCertPath(cmd)
	require.NoError(t, err)
	require.Equal(
		t, filepath.Join(
			"/tmp/darepod", "data", "regtest", "tls.cert",
		),
		tlsCertPath,
	)

	macaroonPath, err := daemonMacaroonPath(cmd)
	require.NoError(t, err)
	require.Equal(
		t, filepath.Join(
			"/tmp/darepod", "data", "regtest", "admin.macaroon",
		),
		macaroonPath,
	)
}

func TestDarepocliExpandsDefaultTildeDataDir(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	cmd := NewRootCmd()
	require.NoError(t, cmd.PersistentFlags().Set("datadir", "~/.darepod"))
	require.NoError(t, cmd.PersistentFlags().Set("network", "mainnet"))

	tlsCertPath, err := daemonTLSCertPath(cmd)
	require.NoError(t, err)
	require.Equal(
		t, filepath.Join(
			home, ".darepod", "data", "mainnet", "tls.cert",
		),
		tlsCertPath,
	)
}

func TestDarepocliAuthPathOverrides(t *testing.T) {
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
