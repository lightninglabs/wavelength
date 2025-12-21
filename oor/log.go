package oor

import (
	"context"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/build"
)

// Subsystem defines the logging code for this subsystem.
const Subsystem = "OORC"

// log is a logger that is initialized with no output filters. This means the
// package will not perform any logging by default until the caller requests
// it.
var log = btclog.Disabled

// DisableLog disables all library log output. Logging output is disabled by
// default until UseLogger is called.
func DisableLog() {
	UseLogger(btclog.Disabled)
}

// UseLogger uses a specified Logger to output package logging info.
func UseLogger(logger btclog.Logger) {
	log = logger
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

// ReportError logs the error using the context logger.
func (r *contextErrorReporter) ReportError(err error) {
	logger := build.LoggerFromContext(r.ctx).WithPrefix(r.prefix)
	logger.Errorf("FSM error: %v", err)
}

// Compile-time check that contextErrorReporter implements ErrorReporter.
var _ protofsm.ErrorReporter = (*contextErrorReporter)(nil)
