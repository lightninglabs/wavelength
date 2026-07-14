package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/lightninglabs/wavelength/db/sqlc"
)

// postMigrationCheck is a function type for a function that performs a
// post-migration check on the database.
type postMigrationCheck func(context.Context, sqlc.Querier) error

var (
	// postMigrationChecks is a map of functions that are run after the
	// database migration with the version specified in the key has been
	// applied. These functions are used to perform additional checks on the
	// database state that are not fully expressible in SQL.
	//
	// NOTE: This is empty for now, but can be populated with custom
	// migration checks as needed.
	//
	//nolint:unused
	postMigrationChecks = map[uint]postMigrationCheck{}
)

// DatabaseBackend is an interface that contains all methods our different
// database backends implement.
type DatabaseBackend interface {
	BatchedQuerier

	WithTx(tx *sql.Tx) *sqlc.Queries
}

// makePostStepCallbacks turns the post migration checks into a map of post
// step callbacks that can be used with the migrate package. The keys of the map
// are the migration versions, and the values are the callbacks that will be
// executed after the migration with the corresponding version is applied.
func makePostStepCallbacks(db DatabaseBackend, log btclog.Logger,
	checks map[uint]postMigrationCheck) map[uint]migrate.PostStepCallback {

	var (
		ctx  = context.Background()
		txDB = NewTransactionExecutor(
			db, func(tx *sql.Tx) sqlc.Querier {
				return db.WithTx(tx)
			}, log,
		)
		writeTxOpts = WriteTxOption()
	)

	postStepCallbacks := make(map[uint]migrate.PostStepCallback)
	for version, check := range checks {
		// Capture the check in a closure.
		checkFn := check

		runCheck := func(m *migrate.Migration, q sqlc.Querier) error {
			log.InfoS(ctx, "Running post-migration check",
				"version", version,
			)
			start := time.Now()

			err := checkFn(ctx, q)
			if err != nil {
				return fmt.Errorf("post-migration check "+
					"failed for version %d: %w", version,
					err)
			}

			log.InfoS(ctx, "Post-migration check completed",
				"version", version,
				"duration", time.Since(start),
			)

			return nil
		}

		// We ignore the actual driver that's being returned here, since
		// we use migrate.NewWithInstance() to create the migration
		// instance from our already instantiated database backend that
		// is also passed into this function.
		postStepCallbacks[version] = func(m *migrate.Migration,
			_ database.Driver) error {

			return txDB.ExecTx(
				ctx, writeTxOpts, func(q sqlc.Querier) error {
					return runCheck(m, q)
				},
			)
		}
	}

	return postStepCallbacks
}
