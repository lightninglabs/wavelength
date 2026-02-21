package serverconn

import (
	"testing"

	"github.com/btcsuite/btclog/v2"
)

// TestLoggerHelpers verifies the package logger helper functions are callable.
// Not parallel — mutates the package-level logger.
func TestLoggerHelpers(t *testing.T) {
	UseLogger(btclog.Disabled)
	DisableLog()
}
