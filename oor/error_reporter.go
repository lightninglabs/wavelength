package oor

import (
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
)

// contextErrorReporter implements protofsm.ErrorReporter by logging errors
// using a provided logger.
type contextErrorReporter struct {
	log btclog.Logger
}

// ReportError logs the error using the configured logger.
func (r *contextErrorReporter) ReportError(err error) {
	r.log.Errorf("FSM error: %v", err)
}

// Compile-time check that contextErrorReporter implements ErrorReporter.
var _ protofsm.ErrorReporter = (*contextErrorReporter)(nil)
