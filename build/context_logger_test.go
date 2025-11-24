package build

import (
	"os"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// TestContextWithLoggerRoundTrip tests that a logger can be stored in a context
// and retrieved successfully.
func TestContextWithLoggerRoundTrip(t *testing.T) {
	t.Parallel()

	// Create a real logger for testing.
	backend := btclog.NewDefaultHandler(os.Stdout)
	logger := btclog.NewSLogger(backend.SubSystem("TEST"))

	// Store the logger in a context.
	ctx := ContextWithLogger(t.Context(), logger)

	// Retrieve the logger from the context.
	retrieved := LoggerFromContext(ctx)

	// Verify we got the same logger back.
	require.Equal(t, logger, retrieved)
}

// TestLoggerFromContextReturnsDisabledWhenMissing tests that LoggerFromContext
// returns btclog.Disabled when no logger is present in the context.
func TestLoggerFromContextReturnsDisabledWhenMissing(t *testing.T) {
	t.Parallel()

	// Use an empty context with no logger.
	ctx := t.Context()

	// LoggerFromContext should return the disabled logger.
	logger := LoggerFromContext(ctx)

	require.Equal(t, btclog.Disabled, logger)
}

// TestLoggerFromContextReturnsDisabledWhenNil tests that LoggerFromContext
// returns btclog.Disabled when a nil logger was stored in the context.
func TestLoggerFromContextReturnsDisabledWhenNil(t *testing.T) {
	t.Parallel()

	// Store a nil logger in the context.
	ctx := ContextWithLogger(t.Context(), nil)

	// LoggerFromContext should return the disabled logger.
	logger := LoggerFromContext(ctx)

	require.Equal(t, btclog.Disabled, logger)
}

// TestMustLoggerFromContextPanicsWhenMissing tests that MustLoggerFromContext
// panics when no logger is present in the context.
func TestMustLoggerFromContextPanicsWhenMissing(t *testing.T) {
	t.Parallel()

	// Use an empty context with no logger.
	ctx := t.Context()

	// MustLoggerFromContext should panic.
	require.Panics(t, func() {
		MustLoggerFromContext(ctx)
	})
}

// TestMustLoggerFromContextPanicsWhenNil tests that MustLoggerFromContext
// panics when a nil logger was stored in the context.
func TestMustLoggerFromContextPanicsWhenNil(t *testing.T) {
	t.Parallel()

	// Store a nil logger in the context.
	ctx := ContextWithLogger(t.Context(), nil)

	// MustLoggerFromContext should panic.
	require.Panics(t, func() {
		MustLoggerFromContext(ctx)
	})
}

// TestMustLoggerFromContextSucceeds tests that MustLoggerFromContext returns
// the logger when one is present.
func TestMustLoggerFromContextSucceeds(t *testing.T) {
	t.Parallel()

	// Create a real logger for testing.
	backend := btclog.NewDefaultHandler(os.Stdout)
	logger := btclog.NewSLogger(backend.SubSystem("TEST"))

	// Store the logger in a context.
	ctx := ContextWithLogger(t.Context(), logger)

	// MustLoggerFromContext should return the logger without panicking.
	retrieved := MustLoggerFromContext(ctx)

	require.Equal(t, logger, retrieved)
}
