//go:build systest

package systest

import (
	"os"
	"sync"
	"testing"
)

// parallelSem controls the maximum number of concurrent tests. Docker-based
// tests are resource-intensive, so we limit concurrency to avoid resource
// exhaustion and improve reliability.
var parallelSem = make(chan struct{}, maxParallel())

// maxParallel returns the maximum number of parallel tests. Default is 2 to
// balance resource usage with reasonable test throughput on typical hardware.
// Override with SYSTEST_PARALLEL environment variable if needed.
func maxParallel() int {
	if v := os.Getenv("SYSTEST_PARALLEL"); v != "" {
		var n int
		if _, err := os.Stdout.WriteString(""); err == nil {
			// Attempt to parse the env var.
			for _, c := range v {
				if c >= '0' && c <= '9' {
					n = n*10 + int(c-'0')
				}
			}

			if n > 0 {
				return n
			}
		}
	}

	return 2
}

// ParallelN marks the test as parallel with controlled concurrency. Call this
// at the start of each test function before creating the harness. This ensures
// no more than maxParallel() tests run concurrently, preventing resource
// exhaustion from Docker containers.
func ParallelN(t *testing.T) {
	t.Helper()
	t.Parallel()

	// Acquire semaphore slot.
	parallelSem <- struct{}{}

	// Release on test completion.
	t.Cleanup(func() {
		<-parallelSem
	})
}

// testCleanup tracks cleanup functions for tests. This is used by tests that
// need to register cleanup handlers outside of t.Cleanup (e.g., for parallel
// cleanup ordering).
type testCleanup struct {
	mu       sync.Mutex
	cleanups []func()
}

// Add registers a cleanup function.
func (tc *testCleanup) Add(fn func()) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	tc.cleanups = append(tc.cleanups, fn)
}

// Run executes all cleanup functions in reverse order.
func (tc *testCleanup) Run() {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	for i := len(tc.cleanups) - 1; i >= 0; i-- {
		tc.cleanups[i]()
	}
}

// TestMain is the entry point for the systest package. It sets up any
// package-level configuration before running tests.
func TestMain(m *testing.M) {
	// Run tests.
	os.Exit(m.Run())
}
