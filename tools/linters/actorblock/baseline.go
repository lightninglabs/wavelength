package actorblock

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Baseline is the set of functions whose blocking call sites are known and
// tolerated. It exists so that the analyzer can be enabled on a tree that
// still has legacy sites: only sites beyond the baseline fail the build, and
// the staleness check ensures the baseline can only shrink.
//
// An entry is the name of the function that directly contains the blocking
// calls, as "pkgpath.Func" or "pkgpath.Type.Method", and a count per kind of
// site, for example
//
//	github.com/org/mod/pkg.Actor.waitFor await=2 # reason
//
// A function that holds more direct sites of a kind than its entry allows is
// reported, so a second Await added to a baselined function is not silent.
// One limit remains: a new call to an already baselined helper from another
// Receive branch adds no site to the helper, so it is not reported.
type Baseline struct {
	entries map[string]baselineEntry
}

// baselineEntry is the tolerated site count per kind for one function.
type baselineEntry struct {
	counts map[string]int
	reason string
}

// NewBaseline creates an empty baseline.
func NewBaseline() *Baseline {
	return &Baseline{entries: make(map[string]baselineEntry)}
}

// Allows reports whether the baseline tolerates count direct sites of the
// kind in the function key. A nil baseline tolerates nothing.
func (b *Baseline) Allows(key, kind string, count int) bool {
	if b == nil {
		return false
	}

	return count <= b.entries[key].counts[kind]
}

// Keys returns the entries of the baseline in sorted order.
func (b *Baseline) Keys() []string {
	if b == nil {
		return nil
	}

	keys := make([]string, 0, len(b.entries))
	for key := range b.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

// ParseBaseline reads a baseline file. Blank lines and lines starting with
// '#' are ignored. Every other line has the form
// "<key> await=<n> send=<n> # <reason>" with at least one count and a
// non-empty reason, so that each tolerated site says why.
func ParseBaseline(r io.Reader) (*Baseline, error) {
	b := NewBaseline()

	scanner := bufio.NewScanner(r)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		spec, reason, found := strings.Cut(line, " #")
		fields := strings.Fields(spec)
		reason = strings.TrimSpace(reason)
		if !found || len(fields) < 2 || reason == "" {
			return nil, fmt.Errorf("baseline line %d: want "+
				"\"<pkgpath.Func> await=<n> # "+
				"<reason>\", got %q", lineNo, line)
		}

		counts := make(map[string]int)
		for _, f := range fields[1:] {
			kind, num, ok := strings.Cut(f, "=")
			n, err := strconv.Atoi(num)
			if !ok || err != nil || n < 1 ||
				(kind != KindAwait && kind != KindSend) {
				return nil, fmt.Errorf("baseline line %d: bad "+
					"count %q", lineNo, f)
			}
			counts[kind] = n
		}

		key := fields[0]
		if _, dup := b.entries[key]; dup {
			return nil, fmt.Errorf("baseline line %d: duplicate "+
				"entry %q", lineNo, key)
		}

		b.entries[key] = baselineEntry{counts: counts, reason: reason}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return b, nil
}

// LoadBaseline reads the baseline file at path. A relative path is looked up
// from the working directory and then from each of its parents, so that a
// linter started from a subdirectory of the repository still finds it.
func LoadBaseline(path string) (*Baseline, error) {
	resolved := path
	if !filepath.IsAbs(path) {
		dir, err := os.Getwd()
		if err != nil {
			return nil, err
		}

		for {
			candidate := filepath.Join(dir, path)
			if _, err := os.Stat(candidate); err == nil {
				resolved = candidate
				break
			}

			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	f, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return ParseBaseline(f)
}

// FormatBaseline renders the baseline file for the given needed sites. The
// map is keyed by function and holds the number of direct sites per kind.
func FormatBaseline(needed map[string]map[string]int) string {
	keys := make([]string, 0, len(needed))
	for key := range needed {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString(baselineHeader)
	for _, key := range keys {
		var specs, reasons []string
		if n := needed[key][KindAwait]; n > 0 {
			specs = append(
				specs, fmt.Sprintf("%s=%d", KindAwait, n),
			)
			reasons = append(
				reasons, "legacy Await in a turn, migrate "+
					"to AskThen or DetachAskPromise",
			)
		}
		if n := needed[key][KindSend]; n > 0 {
			specs = append(specs, fmt.Sprintf("%s=%d", KindSend, n))
			reasons = append(
				reasons, "legacy send with a non-turn "+
					"context, pass the turn context",
			)
		}

		fmt.Fprintf(
			&sb, "%s %s # %s\n", key, strings.Join(specs, " "),
			strings.Join(reasons, "; "),
		)
	}

	return sb.String()
}

const baselineHeader = `# Baseline for the actorblock analyzer.
#
# Each line names a function that still contains blocking calls reachable
# from an actor turn, as "<pkgpath.Func> await=<n> send=<n> # <reason>", with
# the number of direct sites per kind. Entries are sorted. The analyzer fails
# on any site beyond these counts, and the standalone driver fails on any
# entry or count that no longer matches, so this file can only shrink: delete
# a line or lower a count when sites are fixed, and never add or raise one
# for a new site, fix the site or exempt it with a reasoned directive.
#
`

// Stale describes the baseline entries that the findings no longer need, in
// sorted order. needed is the Recorder output of a whole-tree run, so the
// result is only meaningful when every package that can reach the entries
// was analyzed. A count above what the tree holds is stale too and must be
// lowered, since the spare room would let a new site in unnoticed.
func (b *Baseline) Stale(needed map[string]map[string]int) []string {
	var stale []string
	for _, key := range b.Keys() {
		for _, kind := range []string{KindAwait, KindSend} {
			have, ok := b.entries[key].counts[kind]
			if !ok {
				continue
			}

			switch got := needed[key][kind]; {
			case got == 0:
				stale = append(
					stale, fmt.Sprintf("%s: no %s site "+
						"left, remove %s=%d", key,
						kind, kind, have),
				)

			case got < have:
				stale = append(
					stale, fmt.Sprintf("%s: %s count "+
						"shrank, lower %s=%d to %s=%d",
						key, kind, kind, have, kind,
						got),
				)
			}
		}
	}

	return stale
}
