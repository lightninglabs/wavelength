package chainsource

import (
	"context"
	"time"

	"github.com/btcsuite/btclog/v2"
)

// finalityBlockSubscriptionBackoffs is the retry schedule conf/spend
// actors use when arming the block-epoch subscription that drives
// height-based finality synthesis. Three attempts at 100ms, 500ms, 2s
// spread the retries across roughly 2.6 seconds — short enough that a
// transient backend hiccup is absorbed inside the monitoring
// goroutine, long enough that we are not spinning on a backend that
// is genuinely down. Exhaustion is non-fatal: the sub-actor logs and
// falls back to whatever Done the backend itself delivers.
var finalityBlockSubscriptionBackoffs = []time.Duration{
	100 * time.Millisecond,
	500 * time.Millisecond,
	2 * time.Second,
}

// finalityBlockSubscriptionAttemptTimeout bounds each individual
// RegisterBlocks attempt. Without it a single hung RegisterBlocks call
// (e.g. a wedged lndclient gRPC stream) would block the conf/spend
// monitoring goroutine indefinitely — stalling Confirmed/Reorged/Done
// delivery on that watch — since the retry schedule only bounds the gaps
// between attempts, not the attempts themselves. 10s mirrors the per-call
// registration timeout used in conf_actor.go's handleRegisterConf so the
// whole file behaves consistently under a slow backend.
const finalityBlockSubscriptionAttemptTimeout = 10 * time.Second

// registerBlocksForFinality registers a block-epoch subscription used
// to synthesize a Done signal at FinalityDepth past an observed
// confirmation or spend. The call is retried with a short bounded
// backoff because finality synthesis is the only Done source for
// backends that do not write the upstream Done channel (notably
// lndclient over gRPC); a one-shot RegisterBlocks attempt that
// briefly hiccups would leak the per-watch sub-actor indefinitely.
//
// The retries run in the calling sub-actor's monitoring goroutine, so
// brief blocking here is safe: more confirmation/spend events on this
// specific watch are not expected during the retry window (we already
// consumed the one that triggered the arm), and ctx cancellation
// breaks out promptly.
//
// Returns the registration on success, or a non-nil error after
// retries are exhausted. Callers should log the error at warn level
// and continue without height-based synthesis; the backend's own Done
// channel remains the only finality path in that case.
func registerBlocksForFinality(ctx context.Context, backend ChainBackend,
	log btclog.Logger) (*BlockRegistration, error) {

	var lastErr error
	for attempt, backoff := range finalityBlockSubscriptionBackoffs {
		// Bound each attempt so a hung RegisterBlocks cannot wedge the
		// monitoring goroutine; the retry schedule only bounds the gaps
		// between attempts, not a single stuck call.
		attemptCtx, cancel := context.WithTimeout(
			ctx, finalityBlockSubscriptionAttemptTimeout,
		)
		reg, err := backend.RegisterBlocks(attemptCtx)
		cancel()
		if err == nil {
			return reg, nil
		}
		lastErr = err

		log.WarnS(ctx, "RegisterBlocks for finality synthesis failed; "+
			"retrying", err,
			"attempt", attempt+1,
			"max_attempts", len(finalityBlockSubscriptionBackoffs),
			"backoff", backoff,
		)

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return nil, lastErr
}
