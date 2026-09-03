package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/btcsuite/btclog/v2"
	admigration "github.com/lightninglabs/wavelength/db/actordelivery/migrations"
	dbmigrate "github.com/lightninglabs/wavelength/db/migrate"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// transformByteLiterals converts SQLite hex literal formatting in a SQL query
// into Postgres-compatible hex literal formatting if the configured database
// backend is Postgres. In particular, it transforms occurrences of hex literals
// formatted as X'...' into the format '\x...'.
func transformByteLiterals(t *testing.T, db *BaseDB, query string) string {
	if db.Backend() == sqlc.BackendTypePostgres {
		re := regexp.MustCompile(`X'([0-9A-Fa-f]+?)'`)
		query = re.ReplaceAllString(query, `'\x$1'`)
	}

	return query
}

// TestMigrationSteps is a test that illustrates how to test database
// migrations by selectively applying only some migrations, inserting dummy data
// and then applying the remaining migrations.
func TestMigrationSteps(t *testing.T) {
	ctx := t.Context()

	// As a first step, we create a new database migrated to version 1.
	db := NewTestDBWithVersion(t, 1)

	// Insert some test data into the chain_info table.
	//nolint:ll
	insertMainnet := transformByteLiterals(t, db.BaseDB, `
		INSERT INTO chain_info (id, chain_name, genesis_hash) VALUES (
			1, 'mainnet', X'000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f'
		)
	`)
	_, err := db.ExecContext(ctx, insertMainnet)
	require.NoError(t, err)

	// We should be able to query the chain_info table.
	//nolint:ll
	chainInfoQuery := `SELECT chain_name, genesis_hash FROM chain_info WHERE id = 1`

	var chainName string
	var genesisHash []byte
	err = db.QueryRowContext(ctx, chainInfoQuery).Scan(
		&chainName, &genesisHash,
	)
	require.NoError(t, err)
	require.Equal(t, "mainnet", chainName)
	require.Len(t, genesisHash, 32) // Bitcoin genesis hash is 32 bytes.

	// We can insert a new chain info entry.
	//nolint:ll
	insertQuery := transformByteLiterals(t, db.BaseDB, `
		INSERT INTO chain_info (id, chain_name, genesis_hash) VALUES (
			2, 'testnet', X'000000000933ea01ad0ee984209779baaec3ced90fa3f408719526f8d77f4943'
		)
	`)
	_, err = db.ExecContext(ctx, insertQuery)
	require.NoError(t, err)

	// Verify the new entry exists.
	err = db.QueryRowContext(
		ctx, `SELECT chain_name FROM chain_info WHERE id = 2`,
	).Scan(&chainName)
	require.NoError(t, err)
	require.Equal(t, "testnet", chainName)
}

// TestIdempotentReceiveScriptsMigrationPreservesLegacyRows verifies migration
// 17 adds nullable retry metadata without rewriting existing script ownership.
func TestIdempotentReceiveScriptsMigrationPreservesLegacyRows(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	database := NewTestDBWithVersion(t, 16)
	insert := transformByteLiterals(t, database.BaseDB, `
		INSERT INTO owned_receive_scripts (
			pk_script, client_key_id, operator_pubkey, exit_delay,
			source, created_at, last_used_at
		) VALUES (
			X'512001', NULL, X'020101', 144, 0, 1700000000,
			NULL
		)
	`)
	_, err := database.ExecContext(ctx, insert)
	require.NoError(t, err)

	err = database.ExecuteMigrations(TargetLatest)
	require.NoError(t, err)

	var (
		pkScript                []byte
		idempotencyKey          sql.NullString
		registrationLabel       sql.NullString
		registrationExpiresAt   sql.NullInt64
		registrationRPCKey      sql.NullString
		registrationCompletedAt sql.NullInt64
	)
	err = database.QueryRowContext(ctx, `
		SELECT pk_script, idempotency_key, registration_label,
		       registration_expires_at, registration_rpc_key,
		       registration_completed_at
		FROM owned_receive_scripts
		WHERE pk_script = $1
	`, []byte{0x51, 0x20, 0x01}).Scan(
		&pkScript, &idempotencyKey, &registrationLabel,
		&registrationExpiresAt, &registrationRPCKey,
		&registrationCompletedAt,
	)
	require.NoError(t, err)
	require.Equal(t, []byte{0x51, 0x20, 0x01}, pkScript)
	require.False(t, idempotencyKey.Valid)
	require.False(t, registrationLabel.Valid)
	require.False(t, registrationExpiresAt.Valid)
	require.False(t, registrationRPCKey.Valid)
	require.False(t, registrationCompletedAt.Valid)
}

// TestMigrationDowngrade tests that downgrading the database is prevented.
func TestMigrationDowngrade(t *testing.T) {
	// For this test, with the current hard coded latest version.
	db := NewTestDBWithVersion(t, LatestMigrationVersion)

	// We'll now attempt to execute migrations, targeting the latest
	// version. But we'll have the DB think the latest version is actually
	// less than the current version. This simulates downgrading.
	err := db.ExecuteMigrations(TargetLatest, WithLatestVersion(0))
	require.ErrorIs(t, err, dbmigrate.ErrMigrationDowngrade)
}

// TestOOROutgoingReplayMigrationFailsClosed verifies migration 18 binds each
// legacy key to its session without treating an opaque snapshot as canonical
// request data.
func TestOOROutgoingReplayMigrationFailsClosed(t *testing.T) {
	ctx := t.Context()
	db := NewTestDBWithVersion(t, 17)

	insertSession := transformByteLiterals(t, db.BaseDB, `
		INSERT INTO oor_session_registry (
			session_id, actor_id, direction, phase, idempotency_key,
			status, snapshot_data, snapshot_version, flow_version,
			created_at, updated_at
		) VALUES
			(
				X'010201', 'ark-sign-actor', 1,
				'ark_sign_requested', 'ark-sign-key', 0,
				X'aa01', 4, 0, 10, 20
			),
			(
				X'010202', 'submit-actor', 1, 'submit_sent',
				'submit-key', 0, X'aa02', 4, 0,
				10, 20
			),
			(
				X'010203', 'cosigned-actor', 1, 'cosigned',
				'cosigned-key', 0, X'aa03', 4, 0,
				10, 20
			),
			(
				X'010204', 'finalize-actor', 1, 'finalize_sent',
				'finalize-key', 0, X'aa04', 4, 0,
				10, 20
			),
			(
				X'040506', 'terminal-actor', 1, 'completed',
				'legacy-terminal-key', 1, X'ddeeff', 4, 0,
				10, 20
			),
			(
				X'070809', 'local-update-actor', 1,
				'local_vtxo_update',
				'legacy-local-update-key', 0,
				X'112233', 4, 0, 10, 20
			),
			(
				X'0a0b01', 'failed-reused-key-actor', 1,
				'failed', 'legacy-reused-key', 2,
				X'445501', 4, 0, 5, 20
			),
			(
				X'0a0b02', 'live-reused-key-actor', 1,
				'submit_sent', 'legacy-reused-key', 0,
				X'445502', 4, 0, 10, 20
			)
	`)
	_, err := db.ExecContext(ctx, insertSession)
	require.NoError(t, err)

	require.NoError(t, db.ExecuteMigrations(TargetLatest))

	for _, key := range []string{
		"ark-sign-key", "submit-key", "cosigned-key", "finalize-key",
		"legacy-terminal-key", "legacy-local-update-key",
	} {
		var requestData []byte
		err = db.QueryRowContext(
			ctx, `SELECT request_data FROM oor_dispatch_attempts
				WHERE idempotency_key = $1`, key,
		).Scan(&requestData)
		require.NoError(t, err)
		require.Nil(t, requestData)
	}

	var reusedSessionID []byte
	err = db.QueryRowContext(
		ctx, `SELECT session_id FROM oor_dispatch_attempts
			WHERE idempotency_key = 'legacy-reused-key'`,
	).Scan(&reusedSessionID)
	require.NoError(t, err)
	require.Equal(t, []byte{0x0a, 0x0b, 0x02}, reusedSessionID)

	var legacySnapshot []byte
	err = db.QueryRowContext(
		ctx, `SELECT snapshot_data
			FROM oor_session_registry
			WHERE idempotency_key = 'submit-key'`,
	).Scan(&legacySnapshot)
	require.NoError(t, err)
	require.Equal(t, []byte{0xaa, 0x02}, legacySnapshot)
}

// TestOOROutgoingReplayDowngradePreservesSessions verifies migration 18 can
// be removed without rewriting the existing mutable session registry.
func TestOOROutgoingReplayDowngradePreservesSessions(t *testing.T) {
	ctx := t.Context()
	sqlDB := NewTestDB(t)
	store := NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	).NewOORSessionRegistryStore(clock.NewDefaultClock())

	const dispatchKey = "durable-dispatch-key"
	sessionID := sessionHash(0x71)
	record := OORSessionRegistryRecord{
		SessionID:      sessionID,
		ActorID:        "outgoing",
		Direction:      OORSessionDirectionOutgoing,
		Phase:          "submit_sent",
		IdempotencyKey: dispatchKey,
		Status:         OORSessionStatusPending,
		SnapshotData: []byte{
			0x01,
		},
		SnapshotVersion: 5,
		DispatchRequestData: []byte{
			0xaa,
			0xbb,
		},
	}
	require.NoError(t, store.UpsertSession(ctx, record))

	require.NoError(t, sqlDB.ExecuteMigrations(TargetVersion(17)))

	var key sql.NullString
	err := sqlDB.QueryRowContext(
		ctx, `SELECT idempotency_key FROM oor_session_registry
			WHERE session_id = $1`, sessionID[:],
	).Scan(&key)
	require.NoError(t, err)
	require.True(t, key.Valid)
	require.Equal(t, dispatchKey, key.String)

	_, err = sqlDB.ExecContext(ctx, `SELECT 1 FROM oor_dispatch_attempts`)
	require.Error(t, err)
}

// findDBBackupFilePath walks the directory of the given database file path and
// returns the path to the backup file.
func findDBBackupFilePath(t *testing.T, dbFilePath string) string {
	var dbBackupFilePath string
	dir := filepath.Dir(dbFilePath)

	err := filepath.Walk(
		dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			hasSuffix := strings.HasSuffix(info.Name(), ".backup")
			if !info.IsDir() && hasSuffix {
				dbBackupFilePath = path
			}

			return nil
		},
	)
	require.NoError(t, err)

	return dbBackupFilePath
}

// TestSqliteMigrationBackup tests that the sqlite database backup and migration
// functionality works.
//
// In this test we will create a database, populate it with data at an earlier
// version, create a backup during migration, and then verify both the migrated
// and backup databases contain the expected data.
func TestSqliteMigrationBackup(t *testing.T) {
	ctx := t.Context()

	// Create a new database without running migrations.
	dbFileName := filepath.Join(t.TempDir(), "test.db")
	// For tests, use a simple logger that outputs to the test log.
	log := btclog.Disabled
	db, err := NewSqliteStore(&SqliteConfig{
		DatabaseFileName: dbFileName,
		SkipMigrations:   true,
	}, log)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.DB.Close())
	})

	// Run migrations to the latest version.
	err = db.ExecuteMigrations(TargetLatest)
	require.NoError(t, err)

	// Insert some test data.
	//nolint:ll
	insertQuery := transformByteLiterals(t, db.BaseDB, `
		INSERT INTO chain_info (id, chain_name, genesis_hash) VALUES (
			2, 'regtest', X'0f9188f13cb7b2c71f2a335e3a4fc328bf5beb436012afca590b1a11466e2206'
		)
	`)
	_, err = db.ExecContext(ctx, insertQuery)
	require.NoError(t, err)

	// Now close and reopen the database. Since we're already at the latest
	// version, no migration should run. Leave a legacy backup beside the
	// database to verify that the up-to-date path prunes backups left by an
	// older release.
	require.NoError(t, db.DB.Close())
	legacyBackupPath := dbFileName + ".1234.backup"
	require.NoError(
		t,
		os.WriteFile(
			legacyBackupPath, []byte("legacy"), 0o600,
		),
	)

	db2, err := NewSqliteStore(&SqliteConfig{
		DatabaseFileName: dbFileName,
		SkipMigrations:   false,
	}, log)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db2.DB.Close())
	})

	// Verify the data is still present in the database.
	var chainName string
	err = db2.QueryRowContext(
		ctx, `SELECT chain_name FROM chain_info WHERE id = 2`,
	).Scan(&chainName)
	require.NoError(t, err)
	require.Equal(t, "regtest", chainName)

	// Since we're already at the latest version, the old backup should be
	// pruned and no new backup should be created.
	require.NoFileExists(t, legacyBackupPath)
	dbBackupFilePath := findDBBackupFilePath(t, dbFileName)
	require.Empty(
		t, dbBackupFilePath,
		"no backup should be created when already at latest version",
	)
}

// TestDirtySqliteVersion tests that if a migration fails and leaves a SQLite
// database backend in a dirty state, any attempts of re-executing migrations on
// the db will fail with an error indicating that the database is in a dirty
// state.
func TestDirtySqliteVersion(t *testing.T) {
	var (
		testError = errors.New("test error")

		// testPostMigrationChecks is a map that will trigger a
		// migration callback for migration 1 which always returns an
		// error. This is used to simulate a migration that fails and
		// leaves the db in a dirty state.
		testPostMigrationChecks = map[uint]postMigrationCheck{
			1: func(ctx context.Context, q sqlc.Querier) error {
				return testError
			},
		}
	)

	// Create a new SQLite test db but skip migrations initially.
	dbFileName := filepath.Join(t.TempDir(), "test.db")
	// For tests, use a simple logger that outputs to the test log.
	log := btclog.Disabled
	db, err := NewSqliteStore(&SqliteConfig{
		DatabaseFileName: dbFileName,
		SkipMigrations:   true,
	}, log)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.DB.Close())
	})

	// Attempt to execute migrations with a failing callback.
	err = db.ExecuteMigrations(
		db.backupAndMigrate,
		WithPostStepCallbacks(
			makePostStepCallbacks(db, log, testPostMigrationChecks),
		),
	)
	require.ErrorIs(t, err, testError)

	// If we now attempt to execute migrations again, it should fail with an
	// error indicating that the db is in a dirty state.
	err = db.ExecuteMigrations(db.backupAndMigrate)
	require.ErrorContains(t, err, "database is in a dirty state")
}

// TestSqliteStoreRunsActorDeliveryMigrations verifies that the default sqlite
// store startup path applies isolated actor-delivery migrations.
func TestSqliteStoreRunsActorDeliveryMigrations(t *testing.T) {
	dbFileName := filepath.Join(t.TempDir(), "actor_delivery_test.db")
	log := btclog.Disabled

	store, err := NewSqliteStore(&SqliteConfig{
		DatabaseFileName: dbFileName,
	}, log)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.DB.Close())
	})

	var cnt int
	err = store.QueryRowContext(
		t.Context(),
		"SELECT COUNT(*) FROM sqlite_master "+
			"WHERE type='table' AND name=?",
		admigration.DefaultMigrationsTable,
	).Scan(&cnt)
	require.NoError(t, err)
	require.Equal(t, 1, cnt)
}
