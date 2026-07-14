//go:build !js || !wasm

package db

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/require"
)

const (
	testPgUser   = "test"
	testPgPass   = "test"
	testPgDBName = "test"
	testPgRepo   = "mirror.gcr.io/library/postgres"
	PostgresTag  = "15"

	testPgFixtureParallelism = 4
)

var testPgFixtureSem = make(chan struct{}, testPgFixtureParallelism)

// TestPgFixture is a test fixture that starts a Postgres 15 instance in a
// docker container.
type TestPgFixture struct {
	pool     *dockertest.Pool
	resource *dockertest.Resource
	host     string
	port     int

	releaseSlot func()
}

// NewTestPgFixture constructs a new TestPgFixture starting up a docker
// container running Postgres 15. The started container will expire in after
// the passed duration.
func NewTestPgFixture(t testing.TB, expiry time.Duration,
	autoRemove bool) *TestPgFixture {

	releaseSlot := acquireTestPgFixtureSlot()
	success := false
	defer func() {
		if !success {
			releaseSlot()
		}
	}()

	// Use a sensible default on Windows (tcp/http) and linux/osx (socket)
	// by specifying an empty endpoint.
	pool, err := dockertest.NewPool("")
	require.NoError(t, err, "Could not connect to docker")

	// Pulls an image, creates a container based on it and runs it.
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: testPgRepo,
		Tag:        PostgresTag,
		Env: []string{
			fmt.Sprintf("POSTGRES_USER=%v", testPgUser),
			fmt.Sprintf("POSTGRES_PASSWORD=%v", testPgPass),
			fmt.Sprintf("POSTGRES_DB=%v", testPgDBName),
			"listen_addresses='*'",
		},
		Cmd: []string{
			"postgres",
			"-c", "log_statement=all",
			"-c", "log_destination=stderr",
		},
	}, func(config *docker.HostConfig) {
		// Set AutoRemove to true so that stopped container goes away
		// by itself, unless we want to keep it around for debugging.
		config.AutoRemove = autoRemove
		config.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	require.NoError(t, err, "Could not start resource")

	hostAndPort := resource.GetHostPort("5432/tcp")
	parts := strings.Split(hostAndPort, ":")
	host := parts[0]
	port, err := strconv.ParseInt(parts[1], 10, 64)
	require.NoError(t, err)

	fixture := &TestPgFixture{
		host:        host,
		port:        int(port),
		releaseSlot: releaseSlot,
	}
	databaseURL := fixture.GetDSN()
	t.Logf("Connecting to Postgres fixture: %v\n", databaseURL)

	// Tell docker to hard kill the container in "expiry" seconds.
	require.NoError(t, resource.Expire(uint(expiry.Seconds())))

	// Exponential backoff-retry, because the application in the container
	// might not be ready to accept connections yet.
	pool.MaxWait = 120 * time.Second

	// Keep one readiness pool for the full retry window. Opening a new pool
	// on every attempt leaves the replaced pools and their connection
	// opener goroutines alive for the rest of the test process.
	testDB, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	defer func() {
		_ = testDB.Close()
	}()

	err = pool.Retry(testDB.Ping)
	require.NoError(t, err, "Could not connect to docker")

	// Now fill in the rest of the fixture.
	fixture.pool = pool
	fixture.resource = resource

	success = true

	return fixture
}

// acquireTestPgFixtureSlot bounds active Postgres containers in parallel test
// runs. The db package marks most tests parallel, and unbounded docker startup
// can starve CI runners enough that stores observe partially initialized
// schemas.
func acquireTestPgFixtureSlot() func() {
	testPgFixtureSem <- struct{}{}

	var once sync.Once

	return func() {
		once.Do(func() {
			<-testPgFixtureSem
		})
	}
}

// GetDSN returns the DSN (Data Source Name) for the started Postgres node.
func (f *TestPgFixture) GetDSN() string {
	return f.GetConfig().DSN(false)
}

// GetConfig returns the full config of the Postgres node.
func (f *TestPgFixture) GetConfig() *PostgresConfig {
	return &PostgresConfig{
		Host:       f.host,
		Port:       f.port,
		User:       testPgUser,
		Password:   testPgPass,
		DBName:     testPgDBName,
		RequireSSL: false,
	}
}

// TearDown stops the underlying docker container.
func (f *TestPgFixture) TearDown(t testing.TB) {
	err := f.pool.Purge(f.resource)
	if f.releaseSlot != nil {
		f.releaseSlot()
	}
	require.NoError(t, err, "Could not purge resource")
}

// ClearDB clears the database.
func (f *TestPgFixture) ClearDB(t testing.TB) {
	dbConn, err := sql.Open("postgres", f.GetDSN())
	require.NoError(t, err)

	_, err = dbConn.ExecContext(
		t.Context(),
		`DROP SCHEMA IF EXISTS public CASCADE;
		 CREATE SCHEMA public;`,
	)
	require.NoError(t, err)
}
