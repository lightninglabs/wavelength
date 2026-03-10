package build

import (
	"context"

	"github.com/btcsuite/btclog/v2"
)

// loggerKeyType is the unexported key type used to store a btclog.Logger in a
// context. Using a private struct type prevents collisions with other packages.
type loggerKeyType struct{}

// loggerKey is the singleton context key for the logger value.
var loggerKey loggerKeyType

// ContextWithLogger returns a copy of ctx that carries the given logger. This
// is typically called once at server startup so that every downstream call site
// can extract the logger via LoggerFromContext.
func ContextWithLogger(ctx context.Context,
	l btclog.Logger) context.Context {

	return context.WithValue(ctx, loggerKey, l)
}

// LoggerFromContext extracts the btclog.Logger previously stored via
// ContextWithLogger. If the context does not carry a logger,
// btclog.Disabled is returned so callers never need a nil check.
func LoggerFromContext(ctx context.Context) btclog.Logger {
	l, ok := ctx.Value(loggerKey).(btclog.Logger)
	if !ok || l == nil {
		return btclog.Disabled
	}

	return l
}
