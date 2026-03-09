package build

import (
	"context"
	"os"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// TestContextLoggerRoundTrip verifies that a logger stored in a context via
// ContextWithLogger can be retrieved via LoggerFromContext.
func TestContextLoggerRoundTrip(t *testing.T) {
	t.Parallel()

	handler := btclog.NewDefaultHandler(os.Stdout)
	logger := btclog.NewSLogger(handler)

	ctx := ContextWithLogger(t.Context(), logger)
	got := LoggerFromContext(ctx)

	require.Equal(t, logger, got)
}

// TestLoggerFromContextMissing verifies that LoggerFromContext returns
// btclog.Disabled when no logger has been stored in the context.
func TestLoggerFromContextMissing(t *testing.T) {
	t.Parallel()

	got := LoggerFromContext(t.Context())
	require.Equal(t, btclog.Disabled, got)
}

// TestLoggerFromContextNil verifies that a nil logger stored in the context
// is treated as absent, returning btclog.Disabled.
func TestLoggerFromContextNil(t *testing.T) {
	t.Parallel()

	ctx := context.WithValue(t.Context(), loggerKey, nil)
	got := LoggerFromContext(ctx)

	require.Equal(t, btclog.Disabled, got)
}
