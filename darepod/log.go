package darepod

import (
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/build"
)

// Subsystem defines the logging sub-system name for the darepod package.
const Subsystem = "DRPD"

// log is a logger that is initialized with no output filters. This means the
// package will not perform any logging by default until the caller requests it.
var log btclog.Logger

func init() {
	UseLogger(build.NewSubLogger(Subsystem, nil))
}

// UseLogger uses a specified Logger to output package logging info. This
// should be used in preference to SetLogWriter if the caller is also using
// btclog.
func UseLogger(logger btclog.Logger) {
	log = logger
}
