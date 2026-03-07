package chainbackends

import (
	"github.com/btcsuite/btclog/v2"
)

const (
	// Subsystem defines the logging subsystem code for the chainbackends
	// package.
	Subsystem = "CBKD"

	// LndClientSubsystem defines the logging subsystem code for the
	// lndclient adapter components.
	LndClientSubsystem = "LNDC"
)

// log is a logger that is initialized with no output filters. This means the
// package will not perform any logging by default until the caller requests
// it.
var log = btclog.Disabled

// DisableLog disables all library log output. Logging output is disabled by
// default until UseLogger is called.
func DisableLog() {
	UseLogger(btclog.Disabled)
}

// UseLogger uses a specified Logger to output package logging info. This
// should be used in preference to SetLogWriter if the caller is also using
// btclog.
func UseLogger(logger btclog.Logger) {
	log = logger
}

// lndClientLog is the logger for the lndclient adapter components. It is
// separate from the main package logger so that operators can control
// lndclient adapter verbosity independently via the LNDC subsystem tag.
var lndClientLog = btclog.Disabled

// UseLndClientLogger sets the logger for the lndclient adapter subsystem.
func UseLndClientLogger(logger btclog.Logger) {
	lndClientLog = logger
}
