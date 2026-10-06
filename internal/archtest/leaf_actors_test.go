package archtest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const modulePath = "github.com/lightninglabs/wavelength"

// goTags matches the build tags the repository's unit tests run with.
const goTags = "dev"

// leafActors are the packages whose actors other actors may wait on from
// inside a receive turn. A leaf cannot name a caller's type, because its
// transitive imports contain no other actor package. That does not stop
// references from arriving in messages (the txconfirm Subscriber and the
// chainsource NotifyActor are caller supplied), so the guarantee that matters
// is behavioral: a leaf never parks a turn unboundedly on a subscriber
// reference. txconfirm waits on a subscriber Tell from a goroutine bounded by
// terminalNotifyTimeout, and chainsource delivers from monitor goroutines off
// the actor turn. Adding a package here is a statement that in-turn waits on
// it are safe, so it needs the same scrutiny as adding an in-turn wait.
//
// This test covers only the chainsource and txconfirm subset of the actorblock
// analyzer baseline. It says nothing about vtxo-manager or other non-leaf
// waits.
var leafActors = []string{
	"chainsource",
	"txconfirm",
}

// actorPackages is every package that defines an actor, relative to the
// module root. A new actor package must be added here, which forces a
// deliberate decision about whether it may be a dependency of a leaf.
var actorPackages = []string{
	"chainsource",
	"credit",
	"fraud",
	"ledger",
	"metrics",
	"oor",
	"round",
	"sdk/wavewalletdk",
	"serverconn",
	"timeout",
	"txconfirm",
	"unroll",
	"vtxo",
	"wallet",
}

// frameworkPackages define Receive methods but are not actors that wait on
// leaves: the actor and state machine frameworks, an example, and test
// infrastructure. Leaves may depend on the first two.
var frameworkPackages = []string{
	"baselib/actor",
	"baselib/example",
	"baselib/protofsm",
	"internal/actortest",
	"systest",
}

// receiveRe matches the Receive method of an actor behavior.
var receiveRe = regexp.MustCompile(
	`(?m)^func \([^)]+\) Receive\(\s*\w+ context\.Context`,
)

// TestLeafActorsDoNotImportActors fails when a leaf actor package depends,
// directly or transitively, on any actor package other than the leaves
// themselves. The framework packages are allowed.
func TestLeafActorsDoNotImportActors(t *testing.T) {
	t.Parallel()

	for _, pkg := range actorPackages {
		// Fail loudly if the list is stale rather than passing
		// vacuously.
		goList(t, "-f", "{{.ImportPath}}", modulePath+"/"+pkg)
	}

	leaves := make(map[string]struct{})
	for _, leaf := range leafActors {
		leaves[leaf] = struct{}{}
	}

	for _, leaf := range leafActors {
		deps := goListDeps(t, modulePath+"/"+leaf)

		for _, actorPkg := range actorPackages {
			if _, ok := leaves[actorPkg]; ok {
				continue
			}

			_, found := deps[modulePath+"/"+actorPkg]
			require.Falsef(
				t, found, "leaf actor package %s must not "+
					"depend on actor package %s: "+
					"in-turn waits on %s are only safe "+
					"while it cannot name a caller", leaf,
				actorPkg, leaf,
			)
		}
	}
}

// TestActorPackagesAreClassified fails when a package defines an actor
// Receive method but is in neither actorPackages nor frameworkPackages, so a
// new actor package forces a deliberate classification.
func TestActorPackagesAreClassified(t *testing.T) {
	t.Parallel()

	known := make(map[string]struct{})
	for _, p := range actorPackages {
		known[p] = struct{}{}
	}
	for _, p := range frameworkPackages {
		known[p] = struct{}{}
	}

	out := goList(t, "-f", "{{.ImportPath}} {{.Dir}}", "./...")
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		rel := strings.TrimPrefix(
			strings.TrimPrefix(fields[0], modulePath),
			"/",
		)
		if _, ok := known[rel]; ok {
			continue
		}

		files, err := filepath.Glob(filepath.Join(fields[1], "*.go"))
		require.NoError(t, err)

		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			require.NoError(t, err)

			require.Falsef(
				t, receiveRe.Match(src),
				"package %s defines an actor Receive method "+
					"in %s but is not classified: add "+
					"it to actorPackages or "+
					"frameworkPackages", rel,
				filepath.Base(f),
			)
		}
	}
}

// goListDeps returns the transitive, non-test dependencies of a package,
// including the package itself.
func goListDeps(t *testing.T, pkg string) map[string]struct{} {
	t.Helper()

	out := goList(t, "-deps", "-f", "{{.ImportPath}}", pkg)

	deps := make(map[string]struct{})
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			deps[line] = struct{}{}
		}
	}

	return deps
}

// goList runs go list with the unit test build tags and returns stdout. A
// failure includes the stderr of the go command.
func goList(t *testing.T, args ...string) string {
	t.Helper()

	cmd := exec.Command(
		"go", append([]string{"list", "-tags", goTags}, args...)...,
	)
	cmd.Dir = "../.."
	out, err := cmd.Output()

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("go list %v: %v\n%s", args, err, exitErr.Stderr)
	}
	require.NoError(t, err, "go list %v", args)

	return string(out)
}
