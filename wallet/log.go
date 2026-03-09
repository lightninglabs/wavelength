package wallet

import (
	"github.com/btcsuite/btclog/v2"
)

// Subsystem defines the logging sub system name for the wallet package.
const Subsystem = "ARKW"

// log is a logger that is initialized with no output filters. This means the
// package will not perform any logging by default until the caller requests it.
// The wallet package uses fn.Option[btclog.Logger] in configs; this variable
// is maintained for the central subsystem registry via UseLogger.
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
