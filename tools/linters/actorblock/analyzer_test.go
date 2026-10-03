package actorblock

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis/analysistest"
)

const testActorPkg = "example.com/actor"

var testEntryMethods = []string{
	"ProcessEvent@example.com/protofsm.State",
	"Dispatch@example.com/protofsm.ActorOutboxEvent",
}

// factFilter drops analysistest's "unexpected fact" errors. The harness wants
// every exported fact to be matched by a want comment, which would put a
// comment on every helper in the testdata. The facts are exercised instead by
// the cross-package case, which only reports through a fact.
type factFilter struct {
	testing.TB
}

// Errorf forwards every error except the unexpected fact ones.
func (f factFilter) Errorf(format string, args ...any) {
	if strings.Contains(fmt.Sprintf(format, args...), "unexpected fact") {
		return
	}

	f.TB.Errorf(format, args...)
}

// TestAnalyzer runs the analyzer over the testdata packages and checks the
// reported diagnostics against their want comments.
func TestAnalyzer(t *testing.T) {
	cfg := &Config{
		ActorPkg:     testActorPkg,
		EntryMethods: testEntryMethods,
	}
	a := NewAnalyzer(cfg)

	analysistest.Run(
		factFilter{t}, analysistest.TestData(), a,
		"example.com/blocking", "example.com/crossuse",
		"example.com/fsmuse",
	)
}

// TestBaselineSuppression checks that a baselined function stops its sites
// from being reported while other sites still are, and that the recorder
// sees both.
func TestBaselineSuppression(t *testing.T) {
	baseline, err := ParseBaseline(
		strings.NewReader(
			"example.com/baselined.A.legacy await=1 # legacy " +
				"await\n",
		),
	)
	require.NoError(t, err)

	rec := NewRecorder()
	a := NewAnalyzer(&Config{
		ActorPkg: testActorPkg,
		Baseline: baseline,
		Recorder: rec,
	})

	analysistest.Run(
		factFilter{t}, analysistest.TestData(), a,
		"example.com/baselined",
	)

	// The recorder sees both sites, so a driver can tell the baseline
	// entry is still needed.
	require.Equal(t, map[string]map[string]int{
		"example.com/baselined.A.legacy": {KindAwait: 1},
		"example.com/baselined.A.fresh":  {KindAwait: 1},
	}, rec.Needed())
}

// TestBaselineCount checks that a function holding more direct sites than its
// baseline count is reported, and that the recorder reports the count.
func TestBaselineCount(t *testing.T) {
	baseline, err := ParseBaseline(
		strings.NewReader(
			"example.com/counted.A.two await=1 # " +
				"legacy\nexample.com/counted.A.one await=1 " +
				"# legacy\n",
		),
	)
	require.NoError(t, err)

	rec := NewRecorder()
	a := NewAnalyzer(&Config{
		ActorPkg: testActorPkg,
		Baseline: baseline,
		Recorder: rec,
	})

	analysistest.Run(
		factFilter{t}, analysistest.TestData(), a,
		"example.com/counted",
	)
	require.Equal(t, map[string]map[string]int{
		"example.com/counted.A.two": {KindAwait: 2},
		"example.com/counted.A.one": {KindAwait: 1},
	}, rec.Needed())
}

// TestParseBaseline checks the baseline file format.
func TestParseBaseline(t *testing.T) {
	b, err := ParseBaseline(
		strings.NewReader(
			"# comment\n\na.B await=2 # why\nc.D.e await=1 " +
				"send=3 # x\n",
		),
	)
	require.NoError(t, err)
	require.Equal(t, []string{"a.B", "c.D.e"}, b.Keys())
	require.True(t, b.Allows("a.B", KindAwait, 2))
	require.False(t, b.Allows("a.B", KindAwait, 3))
	require.False(t, b.Allows("a.B", KindSend, 1))
	require.True(t, b.Allows("c.D.e", KindSend, 3))
	require.False(t, b.Allows("a.C", KindAwait, 1))

	for _, bad := range []string{
		"a.B # x\n", "a.B await=1\n", "a.B await=0 # x\n",
		"a.B foo=1 # x\n", "a.B await=1 # x\na.B await=1 # y\n",
	} {
		_, err := ParseBaseline(strings.NewReader(bad))
		require.Error(t, err, bad)
	}
}

// TestFormatBaseline checks that generated baselines round trip and are
// sorted.
func TestFormatBaseline(t *testing.T) {
	out := FormatBaseline(map[string]map[string]int{
		"b.F": {KindSend: 1},
		"a.F": {KindAwait: 2, KindSend: 1},
	})

	b, err := ParseBaseline(strings.NewReader(out))
	require.NoError(t, err)
	require.Equal(t, []string{"a.F", "b.F"}, b.Keys())
	require.True(t, b.Allows("a.F", KindAwait, 2))
}

// TestBaselineStale checks that entries and counts no finding depends on are
// reported.
func TestBaselineStale(t *testing.T) {
	b, err := ParseBaseline(
		strings.NewReader(
			"a.Used await=1 # x\na.Gone await=1 # y\na.Shrunk " +
				"await=3 # z\n",
		),
	)
	require.NoError(t, err)

	stale := b.Stale(map[string]map[string]int{
		"a.Used":   {KindAwait: 1},
		"a.Shrunk": {KindAwait: 2},
		"a.New":    {KindSend: 1},
	})
	require.Equal(t, []string{
		"a.Gone: no await site left, remove await=1",
		"a.Shrunk: await count shrank, lower await=3 to await=2",
	}, stale)
}
