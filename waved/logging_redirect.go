package waved

import (
	"context"
	"sync/atomic"

	btclogv1 "github.com/btcsuite/btclog"
	"github.com/btcsuite/btclog/v2"
)

// redirectLogger keeps LND's package-global logger interfaces immutable while
// forwarding calls to the latest daemon's output, levels and shutdown callback.
// Derived loggers are snapshots, just as before a subsequent UseLogger call.
type redirectLogger struct {
	delegate atomic.Pointer[btclog.Logger]
}

var _ btclog.Logger = (*redirectLogger)(nil)

// setLogger publishes an immutable interface value for concurrent readers.
func (l *redirectLogger) setLogger(logger btclog.Logger) {
	l.delegate.Store(&logger)
}

// logger returns the current delegate, or disables logging before daemon setup.
func (l *redirectLogger) logger() btclog.Logger {
	if logger := l.delegate.Load(); logger != nil {
		return *logger
	}

	return btclog.Disabled
}

// Tracef forwards a formatted trace message.
func (l *redirectLogger) Tracef(format string, params ...any) {
	l.logger().Tracef(format, params...)
}

// Debugf forwards a formatted debug message.
func (l *redirectLogger) Debugf(format string, params ...any) {
	l.logger().Debugf(format, params...)
}

// Infof forwards a formatted info message.
func (l *redirectLogger) Infof(format string, params ...any) {
	l.logger().Infof(format, params...)
}

// Warnf forwards a formatted warning message.
func (l *redirectLogger) Warnf(format string, params ...any) {
	l.logger().Warnf(format, params...)
}

// Errorf forwards a formatted error message.
func (l *redirectLogger) Errorf(format string, params ...any) {
	l.logger().Errorf(format, params...)
}

// Criticalf forwards a formatted critical message and its shutdown callback.
func (l *redirectLogger) Criticalf(format string, params ...any) {
	l.logger().Criticalf(format, params...)
}

// Trace forwards a trace message.
func (l *redirectLogger) Trace(args ...any) {
	l.logger().Trace(args...)
}

// Debug forwards a debug message.
func (l *redirectLogger) Debug(args ...any) {
	l.logger().Debug(args...)
}

// Info forwards an info message.
func (l *redirectLogger) Info(args ...any) {
	l.logger().Info(args...)
}

// Warn forwards a warning message.
func (l *redirectLogger) Warn(args ...any) {
	l.logger().Warn(args...)
}

// Error forwards an error message.
func (l *redirectLogger) Error(args ...any) {
	l.logger().Error(args...)
}

// Critical forwards a critical message and its shutdown callback.
func (l *redirectLogger) Critical(args ...any) {
	l.logger().Critical(args...)
}

// TraceS forwards a structured trace message.
func (l *redirectLogger) TraceS(ctx context.Context, msg string, attrs ...any) {
	l.logger().TraceS(ctx, msg, attrs...)
}

// DebugS forwards a structured debug message.
func (l *redirectLogger) DebugS(ctx context.Context, msg string, attrs ...any) {
	l.logger().DebugS(ctx, msg, attrs...)
}

// InfoS forwards a structured info message.
func (l *redirectLogger) InfoS(ctx context.Context, msg string, attrs ...any) {
	l.logger().InfoS(ctx, msg, attrs...)
}

// WarnS forwards a structured warning message.
func (l *redirectLogger) WarnS(ctx context.Context, msg string, err error,
	attrs ...any) {

	l.logger().WarnS(ctx, msg, err, attrs...)
}

// ErrorS forwards a structured error message.
func (l *redirectLogger) ErrorS(ctx context.Context, msg string, err error,
	attrs ...any) {

	l.logger().ErrorS(ctx, msg, err, attrs...)
}

// CriticalS forwards a structured critical message and its shutdown callback.
func (l *redirectLogger) CriticalS(ctx context.Context, msg string, err error,
	attrs ...any) {

	l.logger().CriticalS(ctx, msg, err, attrs...)
}

// Level returns the current delegate's level.
func (l *redirectLogger) Level() btclogv1.Level {
	return l.logger().Level()
}

// SetLevel updates the current delegate's level.
func (l *redirectLogger) SetLevel(level btclogv1.Level) {
	l.logger().SetLevel(level)
}

// SubSystem snapshots the current delegate with an independent subsystem level.
func (l *redirectLogger) SubSystem(tag string) btclog.Logger {
	return l.logger().SubSystem(tag)
}

// WithPrefix snapshots the current delegate, retaining its level inheritance.
func (l *redirectLogger) WithPrefix(prefix string) btclog.Logger {
	return l.logger().WithPrefix(prefix)
}
