package clientconn

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
)

// ingressLoop is the main pull-dispatch-ack loop for a single client. It
// runs in its own goroutine, started from
// ClientConnectionActor.StartIngress. The loop:
//
//  1. Loads persisted ack watermark state from the checkpoint store.
//  2. Continuously pulls envelopes from the server's per-client mailbox
//     (messages sent by the client).
//  3. Dispatches each envelope to the appropriate server-side actor or
//     response waiter.
//  4. Advances the ack watermark only after durable dispatch commits.
//  5. Calls AckUpTo on the remote mailbox to release processed envelopes.
//
// On transient failures, the loop backs off with exponential delay and
// jitter to prevent busy-spinning.
func (a *ClientConnectionActor) ingressLoop(
	ctx context.Context, state AckState,
) {

	defer a.wg.Done()

	a.log.InfoS(ctx, "Ingress loop starting",
		slog.String("mailbox_id", a.cfg.LocalMailboxID))

	var failCount int

	for {
		select {
		case <-ctx.Done():
			a.log.InfoS(ctx, "Ingress loop exiting",
				slog.String("mailbox_id",
					a.cfg.LocalMailboxID))

			return

		default:
		}

		// Step 1: Ack pending dispatches before pulling more. This
		// allows the remote mailbox to garbage-collect already
		// committed envelopes.
		if state.NeedsAck() {
			if err := a.ackRemote(
				ctx, state.AckTarget,
			); err != nil {
				a.log.WarnS(ctx, "AckUpTo failed, retrying",
					err,
					slog.Uint64("ack_target",
						state.AckTarget))

				a.sleepBackoff(ctx, &failCount)

				continue
			}

			state.AdvanceAck()

			if err := a.saveCheckpoint(ctx, state); err != nil {
				a.log.WarnS(ctx,
					"Failed to save checkpoint "+
						"after ack",
					err)

				// Don't reset failCount — if the checkpoint
				// store is persistently down, we want backoff
				// to apply on subsequent iterations rather
				// than spinning at full speed.
				a.sleepBackoff(ctx, &failCount)

				continue
			}

			failCount = 0
		}

		// Step 2: Pull a batch of envelopes from the server's
		// per-client mailbox (messages from this client).
		envelopes, nextCursor, err := a.pullBatch(
			ctx, state.PullCursor,
		)
		if err != nil {
			a.log.WarnS(ctx, "Pull failed, retrying", err,
				slog.Uint64("cursor", state.PullCursor))

			a.sleepBackoff(ctx, &failCount)

			continue
		}

		if len(envelopes) == 0 {
			// Long-poll returned empty. Reset fail count and
			// loop again immediately — the long-poll timeout
			// already provides the delay.
			failCount = 0

			continue
		}

		a.log.DebugS(ctx, "Pulled envelopes",
			slog.Int("count", len(envelopes)),
			slog.Uint64("cursor", state.PullCursor),
			slog.Uint64("next_cursor", nextCursor))

		// Step 3: Dispatch the batch. On partial failure,
		// committedCursor is the inclusive event_seq of the last
		// safely-processed envelope (dispatched or skipped), NOT
		// the failing one — the failing envelope is preserved for
		// retry.
		committedCursor, anyProcessed, dispatchErr :=
			a.dispatchBatch(ctx, envelopes, nextCursor)

		// Any successfully processed envelope proves the client
		// was recently active. Notify the tracker so liveness
		// state is updated.
		if anyProcessed && a.cfg.ActivityMarker != nil {
			a.cfg.ActivityMarker.MarkActive(a.cfg.ClientID)
		}

		if dispatchErr != nil {
			a.log.WarnS(ctx, "Dispatch failed", dispatchErr,
				slog.Uint64("committed_to",
					committedCursor))

			// Advance state past safely-processed envelopes
			// so we don't re-process them. Adding 1 converts
			// from inclusive to exclusive cursor position.
			advanceCursor := committedCursor + 1
			if anyProcessed &&
				advanceCursor > state.PullCursor {

				state.AdvanceDispatch(advanceCursor)
				state.PullCursor = advanceCursor

				if cpErr := a.saveCheckpoint(
					ctx, state,
				); cpErr != nil {
					a.log.WarnS(ctx,
						"Failed to save checkpoint "+
							"after partial dispatch",
						cpErr)
				}
			}

			a.sleepBackoff(ctx, &failCount)

			continue
		}

		// Step 4: Full batch dispatched successfully. Advance
		// state and persist checkpoint.
		state.AdvanceDispatch(committedCursor)
		state.PullCursor = committedCursor

		if err := a.saveCheckpoint(ctx, state); err != nil {
			a.log.WarnS(ctx,
				"Failed to save checkpoint after dispatch",
				err)

			a.sleepBackoff(ctx, &failCount)

			continue
		}

		failCount = 0
	}
}

// pullBatch calls Edge.Pull and returns the envelopes and next cursor.
func (a *ClientConnectionActor) pullBatch(
	ctx context.Context, cursor uint64,
) ([]*mailboxpb.Envelope, uint64, error) {

	waitMs := uint32(a.cfg.PullWaitTimeout.Milliseconds())

	resp, err := a.cfg.Edge.Pull(ctx, &mailboxpb.PullRequest{
		MailboxId:     a.cfg.LocalMailboxID,
		MaxEnvelopes:  a.cfg.PullMaxEnvelopes,
		WaitTimeoutMs: waitMs,
		Cursor:        cursor,
	})
	if err != nil {
		return nil, 0, err
	}

	if resp.Status != nil && !resp.Status.Ok {
		return nil, 0, &statusError{
			Op:     "Pull",
			Status: resp.Status,
		}
	}

	return resp.Envelopes, resp.NextCursor, nil
}

// dispatchBatch iterates envelopes and routes each one to the correct
// destination:
//
//   - KIND_RESPONSE: delivered to the response registry (unary waiters).
//   - KIND_REQUEST/KIND_EVENT: dispatched to a server-side actor via the
//     configured dispatch table.
//
// On success, returns the exclusive batch-next cursor (one past the last
// envelope). On partial failure, returns the inclusive event_seq of the
// last successfully dispatched envelope along with the error. The caller
// must add 1 to the error-path return value to get the exclusive cursor.
//
//nolint:nonamedreturns
func (a *ClientConnectionActor) dispatchBatch(
	ctx context.Context,
	envelopes []*mailboxpb.Envelope,
	batchNextCursor uint64,
) (lastSafe uint64, anyProcessed bool, _ error) {

	// Track the cursor of the last safely-processed envelope. An
	// envelope is "safe" if it was either successfully dispatched
	// to a durable actor, delivered to a response waiter, or
	// intentionally skipped (missing RPC metadata, unknown kind,
	// no registered dispatcher). On the error path, the caller
	// advances the pull cursor to lastSafe+1, which skips
	// already-processed envelopes but preserves the failing
	// envelope for retry.
	lastSafe = uint64(0)

	for _, env := range envelopes {
		if env.Rpc == nil {
			a.log.WarnS(ctx,
				"Skipping envelope without RPC metadata",
				nil,
				slog.Uint64("event_seq", env.EventSeq))

			// Safe to skip — advance past this envelope
			// so it isn't re-pulled on retry.
			if env.EventSeq > lastSafe {
				lastSafe = env.EventSeq
				anyProcessed = true
			}

			continue
		}

		switch env.Rpc.Kind {
		case mailboxpb.RpcMeta_KIND_RESPONSE:
			// Route to response registry for unary RPC waiters.
			// This is not a durable dispatch — the response is
			// consumed immediately by the waiting goroutine.
			corrID := CorrelationID(env.Rpc.CorrelationId)
			if corrID == "" {
				a.log.WarnS(ctx,
					"Response envelope missing "+
						"correlation ID",
					nil,
					slog.Uint64("event_seq",
						env.EventSeq))

				// Safe to skip — no waiter can match
				// this response.
				if env.EventSeq > lastSafe {
					lastSafe = env.EventSeq
				}

				continue
			}

			delivered := a.deliverResponse(corrID, env)
			if !delivered {
				a.log.WarnS(ctx,
					"Failed to deliver response "+
						"envelope",
					nil,
					slog.String("correlation_id",
						string(corrID)),
					slog.Uint64("event_seq",
						env.EventSeq))
			}

		case mailboxpb.RpcMeta_KIND_REQUEST,
			mailboxpb.RpcMeta_KIND_EVENT:

			// Dispatch to server-side actor via the dispatch
			// table. The dispatcher is a closure that does
			// serviceKey.Ref(system).Tell(ctx, msg). A nil
			// error means the target durable actor persisted
			// the message.
			key := mailboxrpc.ServiceMethod{
				Service: env.Rpc.Service,
				Method:  env.Rpc.Method,
			}

			dispatcher, ok := a.cfg.Dispatchers[key]
			if !ok {
				a.log.WarnS(ctx,
					"No dispatcher for service "+
						"method",
					nil,
					slog.String("service",
						env.Rpc.Service),
					slog.String("method",
						env.Rpc.Method),
					slog.Uint64("event_seq",
						env.EventSeq))

				// Safe to skip — route is
				// intentionally unhandled.
				if env.EventSeq > lastSafe {
					lastSafe = env.EventSeq
				}

				continue
			}

			if err := dispatcher(ctx, env); err != nil {
				// Dispatch failed. Return lastSafe
				// (which does NOT include this
				// envelope) so the caller advances
				// past skipped envelopes but
				// preserves this one for retry.
				return lastSafe, anyProcessed, err
			}

		default:
			a.log.WarnS(ctx,
				"Skipping envelope with unknown RPC kind",
				nil,
				slog.Int("kind", int(env.Rpc.Kind)),
				slog.Uint64("event_seq", env.EventSeq))

			// Safe to skip.
			if env.EventSeq > lastSafe {
				lastSafe = env.EventSeq
				anyProcessed = true
			}

			continue
		}

		// Envelope was successfully dispatched or delivered to a
		// response waiter. Advance lastSafe past it and mark
		// that real processing occurred.
		if env.EventSeq > lastSafe {
			lastSafe = env.EventSeq
		}
		anyProcessed = true
	}

	// All envelopes dispatched successfully. Return the batch next
	// cursor which represents the position after all envelopes in
	// this batch.
	if batchNextCursor > lastSafe {
		lastSafe = batchNextCursor
	}

	return lastSafe, true, nil
}

// ackRemote calls Edge.AckUpTo with the given cursor.
func (a *ClientConnectionActor) ackRemote(
	ctx context.Context, cursor uint64,
) error {

	resp, err := a.cfg.Edge.AckUpTo(ctx, &mailboxpb.AckUpToRequest{
		MailboxId: a.cfg.LocalMailboxID,
		Cursor:    cursor,
	})
	if err != nil {
		return err
	}

	if resp.Status != nil && !resp.Status.Ok {
		return &statusError{
			Op:     "AckUpTo",
			Status: resp.Status,
		}
	}

	return nil
}

// loadCheckpoint restores the AckState from the checkpoint store on
// startup. Returns a zero-value AckState if no checkpoint exists.
func (a *ClientConnectionActor) loadCheckpoint(
	ctx context.Context,
) (AckState, error) {

	actorID := DurableActorID(a.cfg.LocalMailboxID)

	checkpoint, err := a.cfg.Store.LoadCheckpoint(ctx, actorID)
	if err != nil {
		return AckState{}, err
	}
	if checkpoint == nil {
		return AckState{}, nil
	}

	var state AckState
	stateReader := bytes.NewReader(checkpoint.StateData)
	if err := state.Decode(stateReader); err != nil {
		return AckState{}, err
	}

	a.log.InfoS(ctx, "Loaded ack checkpoint",
		slog.String("actor_id", actorID),
		slog.Uint64("pull_cursor", state.PullCursor),
		slog.Uint64("dispatch_committed_to",
			state.DispatchCommittedTo),
		slog.Uint64("ack_target", state.AckTarget),
		slog.Uint64("ack_committed_to", state.AckCommittedTo))

	return state, nil
}

// saveCheckpoint persists the AckState to the checkpoint store.
func (a *ClientConnectionActor) saveCheckpoint(
	ctx context.Context, state AckState,
) error {

	var buf bytes.Buffer
	if err := state.Encode(&buf); err != nil {
		return err
	}

	actorID := DurableActorID(a.cfg.LocalMailboxID)

	return a.cfg.Store.SaveCheckpoint(ctx, actor.CheckpointParams{
		ActorID:   actorID,
		StateType: ackStateType,
		StateData: buf.Bytes(),
	})
}

// sleepBackoff sleeps for an exponential backoff duration with jitter,
// respecting context cancellation. The fail count is incremented on entry
// and used to calculate the delay.
func (a *ClientConnectionActor) sleepBackoff(
	ctx context.Context, failCount *int,
) {

	*failCount++
	delay := retryDelay(
		a.cfg.RetryBaseDelay, a.cfg.RetryMaxDelay, *failCount,
	)

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// retryDelay returns an exponential backoff duration with jitter, capped
// at maxDelay. The formula is:
// min(base * 2^attempt, max) * (0.5 + rand(0.5)).
func retryDelay(
	base time.Duration, maxDelay time.Duration, attempt int,
) time.Duration {

	if base <= 0 {
		base = 200 * time.Millisecond
	}
	if maxDelay <= 0 {
		maxDelay = 30 * time.Second
	}

	// Exponential backoff: base * 2^attempt.
	delay := float64(base) * math.Pow(2, float64(attempt-1))
	if delay > float64(maxDelay) {
		delay = float64(maxDelay)
	}

	// Add jitter: multiply by a random factor in [0.5, 1.0).
	// Crypto-grade randomness is not needed for backoff jitter.
	jitter := 0.5 + rand.Float64()*0.5 //nolint:gosec
	delay *= jitter

	return time.Duration(delay)
}

// statusError wraps a mailbox status failure for error reporting.
type statusError struct {
	// Op is the operation that failed (e.g., "Pull", "AckUpTo").
	Op string

	// Status is the status returned by the mailbox edge.
	Status *mailboxpb.Status
}

// Error returns a human-readable error string.
func (e *statusError) Error() string {
	if e.Status == nil {
		return e.Op + ": nil status"
	}

	return e.Op + ": " + e.Status.Message + " (" + e.Status.Code + ")"
}
