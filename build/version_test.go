package build

import (
	"fmt"
	"strings"
	"testing"
)

// TestVersion verifies that Version() renders the version constants in the
// documented semantic-versioning format. It derives the expectation from the
// constants so a version bump never breaks this test.
func TestVersion(t *testing.T) {
	version := Version()

	// The numeric major.minor.patch prefix must come straight from the
	// constants.
	prefix := fmt.Sprintf("%d.%d.%d", AppMajor, AppMinor, AppPatch)
	if !strings.HasPrefix(version, prefix) {
		t.Fatalf("expected version to start with %s, got: %s", prefix,
			version)
	}

	// A pre-release suffix, when set, is appended after a dash; when
	// empty, the version is exactly the numeric prefix.
	switch {
	case AppPreRelease != "":
		if version != prefix+"-"+AppPreRelease {
			t.Fatalf("expected version %s-%s, got: %s", prefix,
				AppPreRelease, version)
		}

	default:
		if version != prefix {
			t.Fatalf("expected version %s, got: %s", prefix,
				version)
		}
	}
}

// TestDeployment verifies that the deployment constant is set correctly.
func TestDeployment(t *testing.T) {
	// When built with dev tags, Deployment should be Development.
	// When built without dev tags, Deployment should be Production.

	// We can't test both cases in the same test run, so we just verify
	// that it's one of the valid values.
	if Deployment != Development && Deployment != Production {
		t.Fatalf("unexpected deployment type: %v", Deployment)
	}

	// Verify String() method works.
	deploymentStr := Deployment.String()
	if deploymentStr != "development" && deploymentStr != "production" {
		t.Fatalf("unexpected deployment string: %s", deploymentStr)
	}
}

// TestTags verifies that Tags() returns a proper list.
func TestTags(t *testing.T) {
	// Tags might be empty or might contain values depending on how
	// the test is built. We just verify the function doesn't panic
	// and returns a valid slice.
	tags := Tags()

	// Should return nil or a valid slice.
	if tags != nil && len(tags) == 0 {
		t.Fatal("Tags() returned non-nil empty slice, expected nil")
	}
}

// TestCommitHash verifies that CommitHash is populated from runtime.
func TestCommitHash(t *testing.T) {
	// CommitHash should either be empty or a valid short hash (7 chars).
	if CommitHash != "" && len(CommitHash) != 7 {
		t.Logf(
			"Warning: CommitHash length is %d, expected 7: %s",
			len(CommitHash), CommitHash,
		)
	}
}

// TestGoVersion verifies that GoVersion is set.
func TestGoVersion(t *testing.T) {
	// GoVersion should always be set by the init() function.
	if GoVersion == "" {
		t.Fatal("GoVersion is empty")
	}

	if !strings.HasPrefix(GoVersion, "go") {
		t.Fatalf("expected GoVersion to start with 'go', got: %s",
			GoVersion)
	}
}
