//go:build systest

package systest

import (
	"os"
	"sync"
	"testing"

	"github.com/lightninglabs/darepo-client/harness"
)

var (
	// sharedHarness is the shared Docker harness for all systests.
	sharedHarness *harness.Harness

	// harnessOnce ensures the harness is only started once.
	harnessOnce sync.Once

	// harnessStarted tracks whether the harness was ever started.
	harnessStarted bool
)

// TestMain is the entry point for the systest suite. It manages the shared
// harness lifecycle - the harness is started lazily on the first test that
// needs it, and stopped after all tests complete.
func TestMain(m *testing.M) {
	// Run all tests.
	code := m.Run()

	// Stop the harness after all tests complete (if it was started).
	if harnessStarted && sharedHarness != nil {
		sharedHarness.Stop()
	}

	os.Exit(code)
}

// GetSharedHarness returns the shared harness, starting it lazily on first
// call. The harness is started using the first test's T for logging but
// cleanup is handled by TestMain, not t.Cleanup.
func GetSharedHarness(t *testing.T) *harness.Harness {
	t.Helper()

	harnessOnce.Do(func() {
		opts := harness.DefaultOptions()
		opts.StartTapd = false // Don't need tapd for boarding tests.
		opts.GroupName = "systest"

		sharedHarness = harness.NewHarness(t, &opts)
		sharedHarness.Start()
		harnessStarted = true

		// NOTE: We intentionally do NOT register t.Cleanup here
		// since TestMain handles harness cleanup after all tests.
	})

	return sharedHarness
}
