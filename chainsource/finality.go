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

// registerBlocksForFinality registers a block-epoch subscription used
// to synthesize a Done signal at FinalityDepth past an observed
// confirmation or spend. The call is retried with a short bounded
// backoff because finality synthesis is the only Done source for
// backends that do not write the upstream Done channel (notably
// lndclient over gRPC); a one-shot RegisterBlocks attempt that
// briefly hiccups would leak the per-watch sub-actor indefinitely.
//
// The retries run in a dedicated arming goroutine (not the sub-actor's
// select loop), so brief blocking here is safe: more confirmation/spend
// events on this specific watch are not expected during the retry window
// (we already consumed the one that triggered the arm), and ctx
// cancellation breaks out promptly.
//
// The passed ctx MUST be the sub-actor's long-lived context, and it is
// handed to RegisterBlocks unwrapped: for in-process backends the
// block-epoch forwarder goroutine is tied to the ctx it receives, so
// bounding each attempt with a cancellable child ctx (and cancelling it
// once the call returns) would tear the subscription down the instant it
// was armed — starving finality synthesis of the very epochs it needs.
// A hung RegisterBlocks can therefore stall this arming goroutine, but
// that is contained: it is off the select loop (fix moved arming there
// precisely so a slow backend cannot wedge Confirmed/Reorged/Done
// delivery), and a genuinely wedged backend is a lost watch regardless.
//
// Returns the registration on success, or a non-nil error after
// retries are exhausted. Callers should log the error at warn level
// and continue without height-based synthesis; the backend's own Done
// channel remains the only finality path in that case.
func registerBlocksForFinality(ctx context.Context, backend ChainBackend,
	log btclog.Logger) (*BlockRegistration, error) {

	var lastErr error
	for attempt, backoff := range finalityBlockSubscriptionBackoffs {
		reg, err := backend.RegisterBlocks(ctx)
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
