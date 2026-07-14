//go:build test_postgres

package db

import (
	"testing"

	admigration "github.com/lightninglabs/wavelength/db/actordelivery/migrations"
	"github.com/stretchr/testify/require"
)

// TestPostgresStoreRunsActorDeliveryMigrations verifies that the default
// Postgres store startup path applies isolated actor-delivery migrations.
func TestPostgresStoreRunsActorDeliveryMigrations(t *testing.T) {
	store := NewTestPostgresDB(t)

	const tableExistsQuery = `
		SELECT COUNT(*)
		FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = $1
	`

	var cnt int
	err := store.QueryRowContext(
		t.Context(),
		tableExistsQuery,
		admigration.DefaultMigrationsTable,
	).Scan(&cnt)
	require.NoError(t, err)
	require.Equal(t, 1, cnt)
}
