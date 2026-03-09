package db

import (
	"github.com/btcsuite/btclog/v2"
)

// Subsystem defines the logging code for this subsystem.
const Subsystem = "DABS"

// log is a logger that is initialized with no output filters. This means the
// package will not perform any logging by default until the caller requests it.
// Store constructors use fn.Option[btclog.Logger] in their configs; when the
// option is None the package-level logger set via UseLogger serves as the
// fallback.
//
//nolint:unused
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
