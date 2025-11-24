package build

import (
	"context"

	"github.com/btcsuite/btclog/v2"
)

// loggerKey is the context key for storing a logger. Using an empty struct as
// the key type ensures that only this package can create keys of this type,
// preventing key collisions with other packages.
type loggerKey struct{}

// ContextWithLogger returns a new context with the given logger attached. This
// allows loggers to be propagated through the call stack via context, reducing
// the need to pass loggers as explicit function parameters.
func ContextWithLogger(ctx context.Context, log btclog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, log)
}

// LoggerFromContext extracts a logger from the context. If no logger is
// present in the context, it returns btclog.Disabled which safely no-ops all
// log calls. This makes it safe to use in any context without nil checks.
func LoggerFromContext(ctx context.Context) btclog.Logger {
	log, ok := ctx.Value(loggerKey{}).(btclog.Logger)
	if !ok || log == nil {
		return btclog.Disabled
	}

	return log
}

// MustLoggerFromContext extracts a logger from the context. If no logger is
// present, it panics. Use this only in code paths where you're certain a logger
// should have been added to the context earlier in the call chain.
func MustLoggerFromContext(ctx context.Context) btclog.Logger {
	log, ok := ctx.Value(loggerKey{}).(btclog.Logger)
	if !ok || log == nil {
		panic("no logger in context")
	}

	return log
}
