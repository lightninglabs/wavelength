//go:build systest

package systest

import (
	"os"
	"testing"
)

// parallelSem controls the maximum number of concurrent tests. Docker-based
// tests are resource-intensive, so we limit concurrency to avoid resource
// exhaustion and improve reliability.
var parallelSem = make(chan struct{}, maxParallel())

// maxParallel returns the maximum number of parallel tests. Default is 1
// because each systest starts multiple Docker-backed daemons and the combined
// startup path is sensitive to resource contention. Override with
// SYSTEST_PARALLEL if a faster machine can run more reliably.
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

	return 1
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

// TestMain is the entry point for the systest package. It sets up any
// package-level configuration before running tests.
func TestMain(m *testing.M) {
	// Run tests.
	os.Exit(m.Run())
}
