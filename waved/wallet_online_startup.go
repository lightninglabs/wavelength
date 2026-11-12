package waved

import (
	"context"
	"sync"
)

// walletOnlineStartup admits policy refresh and persisted-intent replay once.
// A cancelled or failed attempt stays retryable when the host re-enters
// foreground; this coordinator does not schedule its own retry.
type walletOnlineStartup struct {
	mu       sync.Mutex
	complete bool
}

// run serializes callers without treating a failed refresh as initialization.
// Replay is never reached with anonymous terms or after caller cancellation.
func (s *walletOnlineStartup) run(ctx context.Context,
	authenticate func(context.Context) error,
	replay func(context.Context) error) error {

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.complete {
		return nil
	}
	if err := authenticate(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := replay(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.complete = true

	return nil
}

// finishWalletOnlineStartup refreshes authenticated terms before Board replay.
// Host-driven startup calls this only after explicit foreground ingress starts;
// the local recovery hooks already run independently while intake is paused.
func (s *Server) finishWalletOnlineStartup(ctx context.Context) error {
	return s.onlineStartup.run(ctx, func(ctx context.Context) error {
		refreshCtx, cancel := context.WithTimeout(
			ctx, operatorTermsRefreshTimeout,
		)
		defer cancel()

		return s.refreshAuthenticatedOperatorTerms(refreshCtx)
	}, func(ctx context.Context) error {
		err := s.replayPendingIntents(
			ctx, s.walletRef.UnsafeFromSome(),
		)
		if err == nil {
			return nil
		}

		s.log.WarnS(ctx, "Failed to replay pending intents", err)
		// Ordinary daemon startup keeps best-effort replay. A bounded
		// host attempt must remain retryable after any replay failure.
		if s.cfg.HostDrivenIngress || ctx.Err() != nil {
			return err
		}

		return nil
	})
}
