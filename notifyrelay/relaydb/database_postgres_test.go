//go:build test_postgres

package relaydb

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/require"
)

// testOpener starts a disposable PostgreSQL instance with only the relay
// schema. It never connects to a configured publisher or wallet database.
func testOpener(t *testing.T) func() *Database {
	t.Helper()
	pool, err := dockertest.NewPool("")
	require.NoError(t, err)
	pool.MaxWait = 30 * time.Second
	resource, err := pool.Run("postgres", "16-alpine", []string{
		"POSTGRES_USER=relaytest", "POSTGRES_PASSWORD=relaytest",
		"POSTGRES_DB=relaytest",
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Purge(resource)) })
	expiry := time.AfterFunc(3*time.Minute, func() {
		_ = pool.Client.KillContainer(
			docker.KillContainerOptions{
				ID: resource.Container.ID,
			},
		)
	})
	t.Cleanup(func() { expiry.Stop() })
	dsn := fmt.Sprintf("postgres://relaytest:relaytest@localhost:%s/relay"+
		"test?sslmode=disable", resource.GetPort("5432/tcp"))
	require.NoError(
		t, pool.Retry(func() error {
			connection, err := sql.Open("pgx", dsn)
			if err != nil {
				return err
			}
			defer func() {
				_ = connection.Close()
			}()
			ctx, cancel := context.WithTimeout(
				t.Context(), time.Second,
			)
			defer cancel()

			return connection.PingContext(ctx)
		}),
	)

	return func() *Database {
		store, err := OpenPostgres(t.Context(), dsn)
		require.NoError(t, err)

		return store
	}
}
