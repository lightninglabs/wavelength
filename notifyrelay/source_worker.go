package notifyrelay

import (
	"context"
	"log/slog"
	"time"
)

// runFeedSource owns one source connection and steps until daemon cancellation.
// Failures back off without dropping committed jobs or pending ACK evidence.
// All calls and cleanup remain inside the daemon-owned goroutine.
func (s *Server) runFeedSource(ctx context.Context, store *SQLStore,
	id string) {

	attempts := 0
	for ctx.Err() == nil {
		phase := "connect"
		subscriber, err := NewFeedSubscriber(ctx, store, id)
		if err == nil {
			subscriber.ingress.wake = s.wake
			phase = "ingress"
			for ctx.Err() == nil {
				// Scheduling has no authority over source ACK
				// progress. A failed discovery refresh retries
				// on its own cadence.
				if scheduleErr := subscriber.schedule.step(
					ctx, time.Now(),
				); scheduleErr != nil {

					s.log.DebugContext(
						ctx, "Round wake schedule "+
							"refresh deferred",
						slog.String("source", id),
					)
				}
				result, stepErr := subscriber.ingress.next(
					ctx,
				)
				if stepErr != nil {
					break
				}
				if attempts != 0 {
					s.log.InfoContext(
						ctx, "Notification source "+
							"recovered",
						slog.String("source", id),
					)
					attempts = 0
				}
				if result == feedStepLeaseBusy &&
					!waitSource(ctx, time.Second) {

					break
				}
			}
			_ = subscriber.Close()
		}
		if ctx.Err() != nil {
			return
		}
		// No raw driver/network error can expose DSNs, tokens or proof
		// metadata.
		s.sourceFailures.Add(1)
		attempts++
		level := slog.LevelDebug
		if attempts == 1 {
			level = slog.LevelInfo
		}
		s.log.Log(
			ctx, level, "Notification source retry deferred",
			slog.String("source", id),
			slog.String("phase", phase),
			slog.Int("attempt", attempts),
		)
		if !waitSource(ctx, 5*time.Second) {
			return
		}
	}
}

// waitSource is a cancellation-owned delay; no timer escapes the source loop.
func waitSource(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false

	case <-timer.C:
		return true
	}
}
