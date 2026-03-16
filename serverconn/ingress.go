package serverconn

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxconn "github.com/lightninglabs/darepo-client/mailbox/conn"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
)

// ingressLoop is the main pull-dispatch-ack loop. It runs in its own
// goroutine, started from ServerConnectionActor.StartIngress. The loop:
//
//  1. Loads persisted ack watermark state from the checkpoint store.
//  2. Continuously pulls envelopes from the remote mailbox.
//  3. Dispatches each envelope to the appropriate local actor or response
//     waiter.
//  4. Advances the ack watermark only after durable dispatch commits.
//  5. Calls AckUpTo on the remote mailbox to release processed envelopes.
//
// On transient failures, the loop backs off with exponential delay and
// jitter to prevent busy-spinning.
func (a *ServerConnectionActor) ingressLoop(
	ctx context.Context, state AckState,
) {

	defer a.wg.Done()

	log.InfoS(ctx, "Ingress loop starting",
		slog.String("mailbox_id", a.cfg.LocalMailboxID))

	var failCount int

	for {
		select {
		case <-ctx.Done():
			log.InfoS(ctx, "Ingress loop exiting",
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
				log.WarnS(ctx, "AckUpTo failed, retrying",
					err,
					slog.Uint64("ack_target",
						state.AckTarget))

				a.sleepBackoff(ctx, &failCount)

				continue
			}

			state.AdvanceAck()

			if err := a.saveCheckpoint(ctx, state); err != nil {
				log.WarnS(ctx,
					"Failed to save checkpoint after ack",
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

		// Step 2: Pull a batch of envelopes from the remote mailbox.
		envelopes, nextCursor, err := a.pullBatch(
			ctx, state.PullCursor,
		)
		if err != nil {
			log.WarnS(ctx, "Pull failed, retrying", err,
				slog.Uint64("cursor", state.PullCursor))

			a.sleepBackoff(ctx, &failCount)

			continue
		}

		if len(envelopes) == 0 {
			// Long-poll returned empty. Reset fail count and loop
			// again immediately — the long-poll timeout already
			// provides the delay.
			failCount = 0

			continue
		}

		log.DebugS(ctx, "Pulled envelopes",
			slog.Int("count", len(envelopes)),
			slog.Uint64("cursor", state.PullCursor),
			slog.Uint64("next_cursor", nextCursor))

		// Step 3: Dispatch the batch. On partial failure, the
		// committed cursor reflects only the successfully dispatched
		// portion.
		committedCursor, dispatchErr := a.dispatchBatch(
			ctx, envelopes, nextCursor,
		)
		if dispatchErr != nil {
			log.WarnS(ctx, "Dispatch failed", dispatchErr,
				slog.Uint64("committed_to", committedCursor))

			// Even on partial failure, advance state past the
			// last committed envelope so we don't re-dispatch
			// it. dispatchBatch returns the inclusive event_seq
			// of the last successfully dispatched envelope, so
			// we add 1 to get the exclusive next-pull position,
			// consistent with batchNextCursor on the success
			// path.
			nextCursor := committedCursor + 1
			if committedCursor > 0 &&
				nextCursor > state.PullCursor {

				state.AdvanceDispatch(nextCursor)
				state.PullCursor = nextCursor

				if cpErr := a.saveCheckpoint(
					ctx, state,
				); cpErr != nil {
					log.WarnS(ctx,
						"Failed to save checkpoint "+
							"after partial dispatch",
						cpErr)
				}
			}

			a.sleepBackoff(ctx, &failCount)

			continue
		}

		// Step 4: Full batch dispatched successfully. Advance state
		// and persist checkpoint.
		state.AdvanceDispatch(committedCursor)
		state.PullCursor = committedCursor

		if err := a.saveCheckpoint(ctx, state); err != nil {
			log.WarnS(ctx,
				"Failed to save checkpoint after dispatch",
				err)

			a.sleepBackoff(ctx, &failCount)

			continue
		}

		failCount = 0
	}
}

// pullBatch calls Edge.Pull and returns the envelopes and next cursor.
func (a *ServerConnectionActor) pullBatch(
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
//   - KIND_RESPONSE: delivered to the response registry (unary waiters), or
//     durably dispatched via the configured dispatch table when no waiter is
//     registered for the correlation ID.
//   - KIND_REQUEST/KIND_EVENT: dispatched to a local actor via the configured
//     dispatch table.
//
// On success, returns the exclusive batch-next cursor (one past the last
// envelope). On partial failure, returns the inclusive event_seq of the
// last successfully dispatched envelope along with the error. The caller
// must add 1 to the error-path return value to get the exclusive cursor.
func (a *ServerConnectionActor) dispatchBatch(
	ctx context.Context,
	envelopes []*mailboxpb.Envelope,
	batchNextCursor uint64,
) (uint64, error) {

	// Track the cursor of the last successfully dispatched envelope.
	// Start with the current pull cursor as the base.
	lastCommitted := uint64(0)

	for _, env := range envelopes {
		if env.Rpc == nil {
			log.WarnS(ctx, "Skipping envelope without RPC metadata",
				nil,
				slog.Uint64("event_seq", env.EventSeq))

			continue
		}

		switch env.Rpc.Kind {
		case mailboxpb.RpcMeta_KIND_RESPONSE:
			// Prefer unary waiters for low-latency RPC callers. When no
			// in-memory waiter is registered, fall back to the durable
			// dispatch table so actor-driven unary flows can treat the
			// response like any other ingress event.
			corrID := CorrelationID(env.Rpc.CorrelationId)
			if corrID == "" {
				log.WarnS(ctx,
					"Response envelope missing "+
						"correlation ID",
					nil,
					slog.Uint64("event_seq",
						env.EventSeq))

				continue
			}

			delivery := a.deliverResponse(corrID, env)
			if delivery == mailboxconn.DeliveryWaiter {
				break
			}

			dispatcher, ok := a.cfg.Dispatchers[mailboxrpc.ServiceMethod{
				Service: env.Rpc.Service,
				Method:  env.Rpc.Method,
			}]
			if !ok {
				log.WarnS(ctx,
					"Failed to deliver response "+
						"envelope",
					nil,
					slog.String("delivery_result",
						fmt.Sprintf("%d", delivery)),
					slog.String("service", env.Rpc.Service),
					slog.String("method", env.Rpc.Method),
					slog.String("correlation_id",
						string(corrID)),
					slog.Uint64("event_seq",
						env.EventSeq))

				break
			}

			if err := dispatcher(ctx, env); err != nil {
				return lastCommitted, err
			}

			if delivery == mailboxconn.DeliveryBuffered {
				a.removePendingResponse(corrID)
			}

		case mailboxpb.RpcMeta_KIND_REQUEST,
			mailboxpb.RpcMeta_KIND_EVENT:

			// Dispatch to local actor via the dispatch table.
			// The dispatcher is a closure that does
			// serviceKey.Ref(system).Tell(ctx, msg). A nil error
			// means the target durable actor persisted the
			// message.
			key := mailboxrpc.ServiceMethod{
				Service: env.Rpc.Service,
				Method:  env.Rpc.Method,
			}

			dispatcher, ok := a.cfg.Dispatchers[key]
			if !ok {
				log.WarnS(ctx,
					"No dispatcher for service method",
					nil,
					slog.String("service",
						env.Rpc.Service),
					slog.String("method",
						env.Rpc.Method),
					slog.Uint64("event_seq",
						env.EventSeq))

				continue
			}

			if err := dispatcher(ctx, env); err != nil {
				// Dispatch failed. Stop processing the
				// batch and return the last committed
				// cursor.
				return lastCommitted, err
			}

		default:
			log.WarnS(ctx,
				"Skipping envelope with unknown RPC kind",
				nil,
				slog.Int("kind", int(env.Rpc.Kind)),
				slog.Uint64("event_seq", env.EventSeq))

			continue
		}

		// Track the event_seq of the last processed envelope. The
		// batch next cursor is the authoritative cursor to advance
		// to after the full batch succeeds.
		if env.EventSeq > lastCommitted {
			lastCommitted = env.EventSeq
		}
	}

	// All envelopes dispatched successfully. Return the batch next cursor
	// which represents the position after all envelopes in this batch.
	if batchNextCursor > lastCommitted {
		lastCommitted = batchNextCursor
	}

	return lastCommitted, nil
}

// ackRemote calls Edge.AckUpTo with the given cursor.
func (a *ServerConnectionActor) ackRemote(
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

// loadCheckpoint restores the AckState from the checkpoint store on startup.
// Returns a zero-value AckState if no checkpoint exists.
func (a *ServerConnectionActor) loadCheckpoint(
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

	log.InfoS(ctx, "Loaded ack checkpoint",
		slog.String("actor_id", actorID),
		slog.Uint64("pull_cursor", state.PullCursor),
		slog.Uint64("dispatch_committed_to",
			state.DispatchCommittedTo),
		slog.Uint64("ack_target", state.AckTarget),
		slog.Uint64("ack_committed_to", state.AckCommittedTo))

	return state, nil
}

// saveCheckpoint persists the AckState to the checkpoint store.
func (a *ServerConnectionActor) saveCheckpoint(
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
func (a *ServerConnectionActor) sleepBackoff(
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

// retryDelay returns an exponential backoff duration with jitter, capped at
// maxDelay. The formula is: min(base * 2^attempt, max) * (0.5 + rand(0.5)).
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
