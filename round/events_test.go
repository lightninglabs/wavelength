package round

import (
	"context"
	"log/slog"
	"testing"

	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
)

// TestIntentPackageLogAttributes verifies that intent package log attributes
// are emitted as structured fields instead of a single malformed argument.
func TestIntentPackageLogAttributes(t *testing.T) {
	t.Parallel()

	packageAttrs := (&IntentPackage{
		Intents: Intents{
			Boarding: make([]BoardingIntent, 2),
			VTXOs:    make([]types.VTXORequest, 1),
		},
	}).logAttributes()

	handler := &capturingHandler{}
	logger := slog.New(handler)
	logger.InfoContext(
		context.Background(),
		"Starting round assembly from intent package", packageAttrs...,
	)

	require.Equal(
		t, "Starting round assembly from intent package",
		handler.message,
	)
	require.NotContains(t, handler.attrs, "!BADKEY")
	require.Equal(t, int64(2), handler.attrs["boarding_intents"].Int64())
	require.Equal(t, int64(1), handler.attrs["vtxo_requests"].Int64())
	require.Equal(t, int64(0), handler.attrs["forfeits"].Int64())
	require.Equal(t, int64(0), handler.attrs["leaves"].Int64())
	require.Len(t, handler.attrs, 4)
}

// capturingHandler records slog records so tests can inspect structured
// attributes without depending on a text handler's rendered format.
type capturingHandler struct {
	message string
	attrs   map[string]slog.Value
}

// Enabled allows every log record through to the capturing handler.
func (h *capturingHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

// Handle records the message and attributes from the supplied log record.
func (h *capturingHandler) Handle(_ context.Context, record slog.Record) error {
	h.message = record.Message
	h.attrs = make(map[string]slog.Value)
	record.Attrs(func(attr slog.Attr) bool {
		h.attrs[attr.Key] = attr.Value

		return true
	})

	return nil
}

// WithAttrs returns a copy of the handler preloaded with additional attrs.
func (h *capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlerCopy := &capturingHandler{
		message: h.message,
		attrs:   make(map[string]slog.Value),
	}
	for key, value := range h.attrs {
		handlerCopy.attrs[key] = value
	}
	for _, attr := range attrs {
		handlerCopy.attrs[attr.Key] = attr.Value
	}

	return handlerCopy
}

// WithGroup returns the handler unchanged because these tests do not use
// grouped attributes.
func (h *capturingHandler) WithGroup(string) slog.Handler {
	return h
}
