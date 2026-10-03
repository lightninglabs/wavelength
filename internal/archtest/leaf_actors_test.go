package archtest

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const modulePath = "github.com/lightninglabs/wavelength"

// leafActors are the packages whose actors other actors may wait on from
// inside a receive turn. Waiting on an actor is only deadlock free if that
// actor can never send to or wait on its callers, so each leaf must not be
// able to name a caller at all. The import graph is the strongest check
// available: a package that cannot import another cannot hold a reference to
// its actor or use its message types. Adding a package here is a statement that
// in-turn waits on it are safe, so it needs the same scrutiny as adding an
// in-turn wait.
var leafActors = []string{
	"chainsource",
	"txconfirm",
}

// callerActors are the packages that wait on the leaf actors from a receive
// turn, so a leaf must never depend on them, directly or transitively.
var callerActors = []string{
	"fraud",
	"oor",
	"round",
	"unroll",
	"vtxo",
	"wallet",
}

// TestLeafActorsDoNotImportCallers fails when a leaf actor package starts to
// depend on one of the actors that waits on it, which is the first step toward
// a wait cycle through that leaf.
func TestLeafActorsDoNotImportCallers(t *testing.T) {
	t.Parallel()

	for _, leaf := range leafActors {
		deps := goListDeps(t, modulePath+"/"+leaf)

		for _, caller := range callerActors {
			target := modulePath + "/" + caller
			_, found := deps[target]
			require.Falsef(
				t, found, "leaf actor package %s must not "+
					"depend on %s: in-turn waits on %s "+
					"are only safe while it cannot call "+
					"back", leaf, caller, leaf,
			)
		}
	}
}

// goListDeps returns the transitive, non-test dependencies of a package,
// including the package itself.
func goListDeps(t *testing.T, pkg string) map[string]struct{} {
	t.Helper()

	out, err := exec.Command(
		"go", "list", "-deps", "-f", "{{.ImportPath}}", pkg,
	).Output()
	require.NoError(t, err, "go list -deps %s", pkg)

	deps := make(map[string]struct{})
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			deps[line] = struct{}{}
		}
	}

	return deps
}
