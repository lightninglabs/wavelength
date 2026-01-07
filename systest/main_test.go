//go:build systest

package systest

import (
	"flag"
	"os"
	"testing"
)

var (
	// testParallelism controls how many systest tests can run in parallel.
	// Each systest spawns its own Docker harness (bitcoind, lnd, etc.)
	// which can be resource-intensive.
	testParallelism = flag.Int(
		"test.parallelism", 4,
		"maximum number of systest tests to run in parallel",
	)

	// testParallelismSem is a semaphore channel to limit parallel test
	// execution based on testParallelism flag.
	testParallelismSem chan struct{}
)

// ParallelN acquires a slot from the parallelism semaphore and marks the test
// as parallel. This allows controlling how many resource-intensive systest
// tests run concurrently via the -test.parallelism flag.
func ParallelN(t *testing.T) {
	t.Helper()

	t.Parallel()

	testParallelismSem <- struct{}{}
	t.Cleanup(func() {
		<-testParallelismSem
	})
}

// TestMain is the entry point for the systest suite. It handles flag parsing
// and initializes the parallelism semaphore.
func TestMain(m *testing.M) {
	flag.Parse()
	testParallelismSem = make(chan struct{}, *testParallelism)

	os.Exit(m.Run())
}
