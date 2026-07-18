package db

import (
	"github.com/golang-migrate/migrate/v4"
)

const (
	// LatestMigrationVersion is the latest migration version of the
	// database. This is used to implement downgrade protection for the
	// daemon.
	//
	// NOTE: This MUST be updated when a new migration is added.
	LatestMigrationVersion uint = 15
)

// MigrationTarget is a functional option that can be passed to applyMigrations
// to specify a target version to migrate to. `currentDBVersion` is the current
// (migration) version of the database, or None if unknown.
// `maxMigrationVersion` is the maximum migration version known to the driver,
// or None if unknown.
type MigrationTarget func(mig *migrate.Migrate,
	currentDBVersion int, maxMigrationVersion uint) error

var (
	// TargetLatest is a MigrationTarget that migrates to the latest
	// version available.
	TargetLatest = func(mig *migrate.Migrate, _ int, _ uint) error {
		return mig.Up()
	}

	// TargetVersion is a MigrationTarget that migrates to the given
	// version.
	TargetVersion = func(version uint) MigrationTarget {
		return func(mig *migrate.Migrate, _ int, _ uint) error {
			return mig.Migrate(version)
		}
	}
)

// migrateOptions holds options for migration execution.
type migrateOptions struct {
	latestVersion     uint
	postStepCallbacks map[uint]migrate.PostStepCallback
}

// defaultMigrateOptions returns a new migrateOptions instance with default
// settings.
func defaultMigrateOptions() *migrateOptions {
	return &migrateOptions{
		latestVersion:     LatestMigrationVersion,
		postStepCallbacks: make(map[uint]migrate.PostStepCallback),
	}
}

// MigrateOpt is a functional option that can be passed to migrate related
// methods to modify behavior.
type MigrateOpt func(*migrateOptions)

// WithLatestVersion allows callers to override the default latest version
// setting.
func WithLatestVersion(version uint) MigrateOpt {
	return func(o *migrateOptions) {
		o.latestVersion = version
	}
}

// WithPostStepCallbacks is an option that can be used to set a map of
// PostStepCallback functions that can be used to execute a Golang based
// migration step after a SQL based migration step has been executed. The key is
// the migration version and the value is the callback function that should be
// run _after_ the step was executed (but before the version is marked as
// cleanly executed). An error returned from the callback will cause the
// migration to fail and the step to be marked as dirty.
func WithPostStepCallbacks(
	postStepCallbacks map[uint]migrate.PostStepCallback) MigrateOpt {

	return func(o *migrateOptions) {
		o.postStepCallbacks = postStepCallbacks
	}
}
