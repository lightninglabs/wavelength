//go:build (!js || !wasm) && !sqlite_cgo

package migrate

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"testing/fstest"

	golangmigrate "github.com/golang-migrate/migrate/v4"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/stretchr/testify/require"
)

// postStepFS holds four toy migrations. Each step records its version in the
// steps table, so a callback can observe exactly which SQL has run when it
// fires.
func postStepFS() fstest.MapFS {
	return fstest.MapFS{
		"m/000001_a.up.sql": {Data: []byte(
			"CREATE TABLE steps (v INTEGER);INSERT INTO steps " +
				"VALUES (1);",
		)},
		"m/000001_a.down.sql": {
			Data: []byte("DROP TABLE steps;"),
		},
		"m/000002_b.up.sql": {Data: []byte(
			"INSERT INTO steps VALUES (2);",
		)},
		"m/000002_b.down.sql": {Data: []byte(
			"DELETE FROM steps WHERE v = 2;",
		)},
		"m/000003_c.up.sql": {Data: []byte(
			"INSERT INTO steps VALUES (3);",
		)},
		"m/000003_c.down.sql": {Data: []byte(
			"DELETE FROM steps WHERE v = 3;",
		)},
		"m/000004_d.up.sql": {Data: []byte(
			"INSERT INTO steps VALUES (4);",
		)},
		"m/000004_d.down.sql": {Data: []byte(
			"DELETE FROM steps WHERE v = 4;",
		)},
	}
}

func newPostStepDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	return db
}

// recorder builds callbacks for the given versions. Each one records the
// highest SQL step visible when it fires.
type recorder struct {
	db     *sql.DB
	fired  []uint
	maxSQL []int
}

func (r *recorder) callbacks(t *testing.T,
	versions ...uint) map[uint]PostStepCallback {

	cbs := make(map[uint]PostStepCallback)
	for _, v := range versions {
		cbs[v] = func() error {
			var m int
			err := r.db.QueryRow(
				"SELECT MAX(v) FROM steps",
			).Scan(&m)
			require.NoError(t, err)

			r.fired = append(r.fired, v)
			r.maxSQL = append(r.maxSQL, m)

			return nil
		}
	}

	return cbs
}

func runPostStep(db *sql.DB, target Target,
	cbs map[uint]PostStepCallback) error {

	return RunMigrations(
		db, sqlc.BackendTypeSqlite, postStepFS(), "m", target,
		Config{
			DatabaseName:      "sqlite",
			PostStepCallbacks: cbs,
		},
	)
}

func toVersion(v uint) Target {
	return func(m *golangmigrate.Migrate, _ int, _ uint) error {
		return m.Migrate(v)
	}
}

// TestPostStepFreshDB checks that callbacks fire in version order, each right
// after its own SQL step and before the next one.
func TestPostStepFreshDB(t *testing.T) {
	db := newPostStepDB(t)
	r := &recorder{db: db}

	require.NoError(t, runPostStep(db, TargetLatest, r.callbacks(t, 2, 3)))
	require.Equal(t, []uint{2, 3}, r.fired)
	require.Equal(t, []int{2, 3}, r.maxSQL)
}

// TestPostStepResumesFromMiddle checks that a database stopped before a
// callback version runs only the callbacks it still crosses.
func TestPostStepResumesFromMiddle(t *testing.T) {
	db := newPostStepDB(t)
	r := &recorder{db: db}
	cbs := r.callbacks(t, 2, 3)

	require.NoError(t, runPostStep(db, toVersion(1), cbs))
	require.Empty(t, r.fired)

	// Stopped between the two callbacks: only the later one fires.
	require.NoError(t, runPostStep(db, toVersion(2), cbs))
	require.Equal(t, []uint{2}, r.fired)

	require.NoError(t, runPostStep(db, TargetLatest, cbs))
	require.Equal(t, []uint{2, 3}, r.fired)
}

// TestPostStepAlreadyPast checks that an up-to-date database fires nothing.
func TestPostStepAlreadyPast(t *testing.T) {
	db := newPostStepDB(t)
	require.NoError(t, runPostStep(db, TargetLatest, nil))

	r := &recorder{db: db}
	require.NoError(t, runPostStep(db, TargetLatest, r.callbacks(t, 2, 3)))
	require.Empty(t, r.fired)
}

// TestPostStepIgnoresDownSteps checks that stepping down onto a callback
// version never runs the callback.
func TestPostStepIgnoresDownSteps(t *testing.T) {
	db := newPostStepDB(t)
	r := &recorder{db: db}
	cbs := r.callbacks(t, 2)

	require.NoError(t, runPostStep(db, TargetLatest, cbs))
	require.Equal(t, []uint{2}, r.fired)

	require.NoError(t, runPostStep(db, toVersion(2), cbs))
	require.Equal(t, []uint{2}, r.fired)
}

// TestPostStepFailureLeavesDirty checks the failure mode: the SQL step stays
// applied, the version is dirty at the failing step, later steps do not run,
// the error is returned, and a retry refuses to start.
func TestPostStepFailureLeavesDirty(t *testing.T) {
	db := newPostStepDB(t)
	boom := errors.New("boom")

	cbs := map[uint]PostStepCallback{
		2: func() error { return boom },
	}
	err := runPostStep(db, TargetLatest, cbs)
	require.ErrorIs(t, err, boom)

	var maxStep int
	require.NoError(
		t, db.QueryRow("SELECT MAX(v) FROM steps").Scan(&maxStep),
	)
	require.Equal(t, 2, maxStep)

	var version int
	var dirty bool
	require.NoError(
		t, db.QueryRow(
			"SELECT version, dirty FROM schema_migrations",
		).Scan(&version, &dirty),
	)
	require.Equal(t, 2, version)
	require.True(t, dirty)

	err = runPostStep(db, TargetLatest, nil)
	require.ErrorContains(t, err, "dirty state")
}
