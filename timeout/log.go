package timeout

import (
	"context"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/build"
)

// Subsystem defines the logging code for this subsystem.
const Subsystem = "TMOT"

// logger returns the logger the actor was configured with, falling back to
// whatever the calling context carries.
//
// The injected logger is the one that actually works in the daemon. An
// actor's lifecycle context descends from context.Background() rather than
// from the actor system's logger-carrying context, so a receive loop that
// resolves its logger from ctx alone writes every line to btclog.Disabled.
// Everything this actor reports about failed deliveries would then be
// invisible in a running daemon, which is exactly when someone needs to read
// it.
func (a *Actor) logger(ctx context.Context) btclog.Logger {
	if a.log != btclog.Disabled {
		return a.log
	}

	return build.LoggerFromContext(ctx)
}
