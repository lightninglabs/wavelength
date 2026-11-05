package btcwbackend

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	neutrinosqldb "github.com/lightninglabs/neutrino/sqldb"
	"github.com/stretchr/testify/require"
)

// TestNeutrinoHeadersImportConfig verifies that btcwallet fast-sync header
// sources are validated and mapped into neutrino's import configuration.
func TestNeutrinoHeadersImportConfig(t *testing.T) {
	t.Parallel()

	cfg, err := neutrinoHeadersImportConfig("", "")
	require.NoError(t, err)
	require.Nil(t, cfg)

	cfg, err = neutrinoHeadersImportConfig("blocks.bin", "filters.bin")
	require.NoError(t, err)
	require.Equal(t, "blocks.bin", cfg.BlockHeadersSource)
	require.Equal(t, "filters.bin", cfg.FilterHeadersSource)
	require.Equal(t, blockchain.BFFastAdd, cfg.ValidationFlags)
}

// TestNewNeutrinoServiceRequiresChainParams checks that invalid chain params
// fail before the service touches filesystem or database resources.
func TestNewNeutrinoServiceRequiresChainParams(t *testing.T) {
	t.Parallel()

	_, err := NewNeutrinoService(
		t.TempDir(), nil, nil, nil, false, "", "", nil,
	)
	require.ErrorContains(t, err, "chain params are required")
}

// TestNeutrinoHeadersImportConfigRequiresBothSources checks that a partial
// import configuration fails before neutrino opens the P2P backend.
func TestNeutrinoHeadersImportConfigRequiresBothSources(t *testing.T) {
	t.Parallel()

	_, err := neutrinoHeadersImportConfig("blocks.bin", "")
	require.ErrorContains(t, err, "must be specified together")

	_, err = neutrinoHeadersImportConfig("", "filters.bin")
	require.ErrorContains(t, err, "must be specified together")
}

// TestNeutrinoSQLiteImportsLegacyState starts neutrino on bbolt, then restarts
// it on the same data dir with the SQLite backend and checks that neutrino's
// one-time legacy import ran and was recorded.
func TestNeutrinoSQLiteImportsLegacyState(t *testing.T) {
	dataDir := t.TempDir()
	params := &chaincfg.RegressionNetParams
	ctx := t.Context()

	start := func(opts ...NeutrinoServiceOption) {
		svc, err := NewNeutrinoService(
			dataDir, params, nil, nil, false, "", "",
			btclog.Disabled,
			append(opts, WithoutGlobalDependencyLoggers())...,
		)
		require.NoError(t, err)
		require.NoError(t, svc.Start(ctx))
		//nolint:contextcheck // Stop owns its shutdown context.
		require.NoError(t, svc.Stop())
	}

	// The first run only creates the legacy bbolt and flat-file state.
	start()
	require.NoFileExists(
		t, filepath.Join(dataDir, neutrinosqldb.DefaultSqliteFilename),
	)

	// The second run opens the same bbolt database with SQLite enabled.
	start(WithNeutrinoSQLite(true))

	dbPath := filepath.Join(dataDir, neutrinosqldb.DefaultSqliteFilename)
	require.FileExists(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	var version int
	var dirty bool
	require.NoError(
		t, db.QueryRow(
			"SELECT version, dirty FROM neutrino_migrations",
		).Scan(&version, &dirty),
	)
	require.GreaterOrEqual(t, version, 2)
	require.False(t, dirty)

	// A further run on the already migrated store must start cleanly.
	start(WithNeutrinoSQLite(true))
}
