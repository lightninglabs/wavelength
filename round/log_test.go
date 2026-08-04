package round

import (
	"bytes"
	"errors"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// TestErrorReporterLogsThroughInjectedLogger is the regression test for FSM
// errors being invisible in a running daemon. The reporter used to resolve its
// logger from a stored context, and the context a round FSM runs under is the
// actor's lifecycle context, which descends from context.Background() and
// carries no logger at all. Every "FSM error" therefore went to
// btclog.Disabled. Reporting now writes to the logger the reporter was built
// with, so this test fails the moment that goes back to a context lookup.
func TestErrorReporterLogsThroughInjectedLogger(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	handler := btclog.NewDefaultHandler(&buf, btclog.WithNoTimestamp())
	handler.SetLevel(btclog.LevelError)

	log := btclog.NewSLogger(handler).WithPrefix("(round=deadbeef)")

	reporter := newLoggerErrorReporter(log)
	reporter.ReportError(errors.New("tree signature mismatch"))

	logged := buf.String()
	require.Contains(t, logged, "FSM error")
	require.Contains(t, logged, "tree signature mismatch")

	// The prefix the caller baked into the logger has to survive, since
	// that is what ties an FSM error to the round that produced it.
	require.Contains(t, logged, "(round=deadbeef)")
}
