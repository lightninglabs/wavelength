package chainsource

import (
	"context"
	"time"

	"github.com/btcsuite/btclog/v2"
)

const (
	// finalityArmInitialBackoff is the first delay between RegisterBlocks
	// attempts when arming the block-epoch subscription that drives
	// height-based finality synthesis. It doubles on each failure up to
	// finalityArmMaxBackoff.
	finalityArmInitialBackoff = 100 * time.Millisecond

	// finalityArmMaxBackoff caps the retry delay. Arming is retried
	// indefinitely (until the watch's context is cancelled) rather than
	// abandoned after a fixed count: for gRPC lndclient and lwwallet,
	// height synthesis is the ONLY source of the terminal Done, so giving
	// up would strand the round/exit in ProvisionallyConfirmed forever
	// (a single-confirmation tx has no later event to trigger a re-arm).
	// A brief backend outage at the arming moment therefore only delays
	// finality, never permanently disables it. 30s stops a genuinely-down
	// backend from spinning while bounding recovery latency once it heals.
	finalityArmMaxBackoff = 30 * time.Second

	// finalityArmEscalateAfter is the consecutive-failure count past which
	// the retry log escalates to an operator-visible "finality stalled"
	// warning, so a persistently unarmable watch is detectable rather than
	// silently stuck in provisional.
	finalityArmEscalateAfter = 5
)

// finalityArmResult is handed from a conf/spend sub-actor's off-loop finality
// arming goroutine back to its select loop. reg is the block-epoch
// subscription (nil only when the watch context was cancelled). height is the
// best chain height observed at arm time (zero if it could not be read); the
// loop uses it to synthesize Done immediately when the arming
// confirmation/spend is already buried past FinalityDepth, rather than
// hanging until a fresh block epoch arrives (the subscription only delivers
// FUTURE epochs).
type finalityArmResult struct {
	reg    *BlockRegistration
	height int32
}

// registerBlocksForFinality registers a block-epoch subscription used
// to synthesize a Done signal at FinalityDepth past an observed
// confirmation or spend. It retries RegisterBlocks indefinitely with a
// capped exponential backoff (until the passed context is cancelled)
// because finality synthesis is the only Done source for backends that
// do not write the upstream Done channel (notably lndclient over gRPC and
// lwwallet). Abandoning the arm after a fixed number of attempts would
// leak the per-watch sub-actor AND strand the round/exit in
// ProvisionallyConfirmed forever if the backend merely hiccups at the
// arming moment: a single-confirmation tx has no later event to trigger a
// re-arm, so the watch would only recover on a daemon restart.
//
// The retries run in a dedicated arming goroutine (not the sub-actor's
// select loop), so blocking here is safe: more confirmation/spend events
// on this specific watch are not expected during the retry window (we
// already consumed the one that triggered the arm), and ctx cancellation
// breaks out promptly. The goroutine is bounded by the watch's lifetime.
//
// The passed ctx MUST be the sub-actor's long-lived context, and it is
// handed to RegisterBlocks unwrapped: for in-process backends the
// block-epoch forwarder goroutine is tied to the ctx it receives, so
// bounding each attempt with a cancellable child ctx (and cancelling it
// once the call returns) would tear the subscription down the instant it
// was armed — starving finality synthesis of the very epochs it needs.
//
// Returns the registration on success, or a non-nil error only when the
// context is cancelled (the watch is shutting down).
func registerBlocksForFinality(ctx context.Context, backend ChainBackend,
	log btclog.Logger) (*BlockRegistration, error) {

	backoff := finalityArmInitialBackoff
	for attempt := 1; ; attempt++ {
		reg, err := backend.RegisterBlocks(ctx)
		if err == nil {
			if attempt > 1 {
				log.InfoS(ctx, "Finality block subscription "+
					"armed after retries",
					"attempts", attempt,
				)
			}

			return reg, nil
		}

		// Height synthesis is the only finality source for gRPC
		// lndclient / lwwallet, so a persistent arming failure stalls
		// the round/exit in provisional. Escalate the log once retries
		// pass finalityArmEscalateAfter so the stall is
		// operator-visible rather than silent; the capped backoff
		// throttles it.
		if attempt >= finalityArmEscalateAfter {
			log.WarnS(ctx, "Finality block subscription arming "+
				"persistently failing; round/exit finality is "+
				"stalled until the backend recovers", err,
				"attempts", attempt,
				"backoff", backoff,
			)
		} else {
			log.WarnS(ctx, "RegisterBlocks for finality synthesis "+
				"failed; retrying", err,
				"attempt", attempt,
				"backoff", backoff,
			)
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		backoff *= 2
		if backoff > finalityArmMaxBackoff {
			backoff = finalityArmMaxBackoff
		}
	}
}
