package timeout

import (
	"context"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/build"
)

// Subsystem defines the logging code for this subsystem.
const Subsystem = "TMOT"

// logger resolves the logger attached to ctx, falling back to
// btclog.Disabled when no logger was wired into the actor runtime.
func logger(ctx context.Context) btclog.Logger {
	return build.LoggerFromContext(ctx)
}
