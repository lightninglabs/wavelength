//go:build itest

package itest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// shardPatterns must stay in lockstep with ITEST_SHARD_PATTERN
// branches in the top-level Makefile. Every TestXxx in this
// package must match exactly one shard regex.
//
// The exits-leave-sweep and fraud shards are a finer-grained
// split of exits-fraud and are what the CI matrix actually runs;
// the exits-fraud regex stays defined for local use (a single
// invocation that covers both halves) but is intentionally not
// counted in the coverage check.
var shardPatterns = map[string]*regexp.Regexp{
	"boarding-fees": regexp.MustCompile(
		`^TestBoarding|^TestFees|^TestVHTLC`,
	),
	"oor-sends": regexp.MustCompile(
		`^TestOOR|^TestRefresh|^TestSend|^TestDirected|^TestSeal`,
	),
	"exits-leave-sweep": regexp.MustCompile(
		`^TestUnilateral|^TestPartial|^TestSweep|^TestLeave`,
	),
	"fraud": regexp.MustCompile(
		`^TestFraud|^TestRecipientFraud`,
	),
}

// TestShardCoverage walks the itest package source and asserts
// every top-level TestXxx function lands in exactly one CI shard.
// This guards against a future test addition that names itself
// outside the established prefixes and would otherwise silently
// skip CI without anyone noticing.
func TestShardCoverage(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	var testNames []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, "_test.go") {
			continue
		}

		// Parse only the test files; the shard partitioning
		// is concerned with what `go test` discovers, which
		// is exactly the set of top-level TestXxx funcs in
		// these files.
		f, err := parser.ParseFile(
			fset, filepath.Join(".", name), nil,
			parser.ParseComments,
		)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}

			// Receiver-bound methods are never test
			// entrypoints regardless of name.
			if fn.Recv != nil {
				continue
			}

			if !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}

			// `TestMain` is the package entrypoint, not
			// a test case.
			if fn.Name.Name == "TestMain" {
				continue
			}

			// This very test is the coverage assertion;
			// it has no business-logic shard and is
			// intentionally pinned to a shard via the
			// Makefile (see `^TestShard` in the
			// boarding-fees branch).
			if fn.Name.Name == "TestShardCoverage" {
				continue
			}

			testNames = append(testNames, fn.Name.Name)
		}
	}

	if len(testNames) == 0 {
		t.Fatal(
			"no Test* functions discovered; refusing to trust " +
				"an empty shard coverage assertion",
		)
	}

	var orphans, doubles []string
	for _, name := range testNames {
		hits := 0
		for _, re := range shardPatterns {
			if re.MatchString(name) {
				hits++
			}
		}
		switch {
		case hits == 0:
			orphans = append(orphans, name)

		case hits > 1:
			doubles = append(doubles, name)
		}
	}

	if len(orphans) > 0 {
		t.Errorf("%d test(s) match no shard pattern; either rename "+
			"them or extend ITEST_SHARD_PATTERN in Makefile and "+
			"shardPatterns here: %v", len(orphans), orphans)
	}

	if len(doubles) > 0 {
		t.Errorf("%d test(s) match more than one shard pattern; the "+
			"suite would run them in every shard: %v", len(doubles),
			doubles)
	}
}
