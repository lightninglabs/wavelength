package round

import (
	"context"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
)

// Subsystem defines the logging code for this subsystem.
const Subsystem = "ROND"

// loggerErrorReporter implements protofsm.ErrorReporter by logging errors to
// the logger it was handed at construction.
//
// The logger is held rather than resolved from a context because the context
// an FSM runs under is the round actor's lifecycle context, which descends
// from context.Background() and so carries no logger at all. Resolving from
// it sent every FSM error to btclog.Disabled: the code looked observable and
// was not.
type loggerErrorReporter struct {
	log btclog.Logger
}

// newLoggerErrorReporter creates an error reporter that logs to the given
// logger. Callers pass the same prefixed logger they give the FSM itself, so
// an FSM error lands beside the transitions that led to it.
func newLoggerErrorReporter(log btclog.Logger) *loggerErrorReporter {
	return &loggerErrorReporter{log: log}
}

// ReportError logs the error using structured logging.
func (r *loggerErrorReporter) ReportError(err error) {
	// ReportError takes no context, and the FSM's own context carries
	// nothing worth propagating here, so the log call gets a bare one.
	r.log.ErrorS(context.Background(), "FSM error", err)
}

// Compile-time check that loggerErrorReporter implements ErrorReporter.
var _ protofsm.ErrorReporter = (*loggerErrorReporter)(nil)
