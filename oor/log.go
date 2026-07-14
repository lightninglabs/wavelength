package oor

import (
	"context"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/build"
)

// Subsystem defines the logging code for this subsystem.
const Subsystem = "OORC"

func logger(ctx context.Context) btclog.Logger {
	return build.LoggerFromContext(ctx)
}

// contextErrorReporter implements protofsm.ErrorReporter by logging errors
// using a logger from the context with a specific prefix.
//
//nolint:containedctx
type contextErrorReporter struct {
	ctx    context.Context
	prefix string
}

// newContextErrorReporter creates an error reporter that logs using the logger
// from the given context with the specified prefix.
func newContextErrorReporter(ctx context.Context,
	prefix string) *contextErrorReporter {

	return &contextErrorReporter{ctx: ctx, prefix: prefix}
}

// ReportError logs the error using the context logger with structured logging.
func (r *contextErrorReporter) ReportError(err error) {
	logger := build.LoggerFromContext(r.ctx).WithPrefix(r.prefix)
	logger.ErrorS(r.ctx, "FSM error", err)
}

// Compile-time check that contextErrorReporter implements ErrorReporter.
var _ protofsm.ErrorReporter = (*contextErrorReporter)(nil)
