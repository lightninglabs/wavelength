package serverconn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/serverconn/mailboxpull"
)

// redriveState is what the ingress loop remembers across the re-pulls of one
// backpressure episode. Both fields exist to keep a redrive from repeating work
// the previous cycle already did: the loop re-pulls the same window every
// backoff cycle for as long as a target stays wedged, and the pre-transaction
// half of a folded dispatch is not idempotent from the operator's point of
// view.
type redriveState struct {
	// deferredSeq is the event_seq of the envelope a full mailbox turned
	// away last cycle, or zero when no deferral is outstanding. It clamps
	// the next redrive to the envelopes at or before it: nothing past it
	// can commit until that one is delivered, so handling the rest again is
	// pure duplicate effort.
	deferredSeq uint64

	// servedNonTxSeq is the highest event_seq of a hoisted request this
	// loop has already answered over the network. A request beyond the
	// cursor a deferred cycle commits is served optimistically — the send
	// has to precede the commit, or a crash in between would advance the
	// cursor past a request nobody answered — and it then stays in the pull
	// window until the cursor reaches it. This watermark is what makes that
	// one serve instead of one per cycle.
	servedNonTxSeq uint64
}

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
func (a *ServerConnectionActor) ingressLoop(ctx context.Context,
	state AckState) {

	defer a.wg.Done()

	a.log.InfoS(ctx, "Ingress loop starting",
		slog.String("mailbox_id", a.cfg.LocalMailboxID),
	)

	var failCount int

	// When the delivery store supports transactions, each pulled batch is
	// dispatched and checkpointed in ONE write transaction below. The
	// ack watermark then rides along with the next dispatch checkpoint
	// instead of paying its own commit; ackDirty tracks the in-memory
	// advance until some checkpoint persists it.
	txStore, txOK := a.cfg.Store.(actor.TxAwareDeliveryStore)
	var ackDirty bool

	// episode tracks an open backpressure episode, so a target that has
	// stopped draining is logged on an interval rather than on every
	// re-pull. It lives here, on the single ingress goroutine's stack,
	// because that is the only place that sees every dispatch outcome in
	// order.
	var episode deferralEpisode

	// deferFailCount is the redrive schedule for backpressure, kept apart
	// from the transport failCount above because the two failures want
	// opposite cadences. A black-holed connection should be retried slowly;
	// a full local mailbox should be redriven as soon as it might have
	// room, because the redrive is the only thing draining the client's
	// inbound backlog. Sharing one counter made a recovering target drain
	// at one pull per RetryMaxDelay.
	var deferFailCount int

	// redrive is what this loop remembers about a deferred cycle so the
	// next one does not repeat its work. It lives here for the same reason
	// as episode: only this goroutine sees every dispatch outcome in order.
	// A fresh ingressLoop starts with a fresh one, which is the right scope
	// — after a restart the cursor is the only surviving state and
	// redelivery is the documented behaviour.
	var redrive redriveState

	for {
		select {
		case <-ctx.Done():
			a.logIngressExit(ctx)

			return

		default:
		}

		// Step 1: Ack pending dispatches before pulling more so the
		// remote mailbox can garbage-collect committed envelopes.
		if exit, retry := a.ackPhase(
			ctx, &state, &ackDirty, &failCount, txOK,
		); exit {
			return
		} else if retry {
			continue
		}

		// Step 2: Pull a batch of envelopes from the remote mailbox.
		envelopes, nextCursor, exit, retry := a.pullPhase(
			ctx, &state, &ackDirty, &failCount,
		)
		if exit {
			return
		} else if retry {
			continue
		}

		// Step 3 (transactional path): deliver in-memory responses
		// outside the transaction, then fold the durable dispatches
		// and the advanced watermark into one commit.
		if txOK {
			newState, foldErr := a.runFoldedDispatch(
				ctx, txStore, envelopes, nextCursor, state,
				&redrive,
			)
			if foldErr != nil {
				// A full target mailbox is backpressure, not a
				// failure. The commit still covered the prefix
				// that was delivered, so the advanced state is
				// adopted and the re-pull resumes at the
				// undelivered envelope instead of redelivering
				// everything ahead of it.
				deferral := a.noteDispatchDeferral(
					ctx, foldErr, &episode,
				)
				if deferral != nil {
					// A cursor that moved means the last
					// redrive delivered part of the
					// backlog, which is progress and not a
					// failure to back off from: the
					// schedule starts over so a draining
					// target is redriven promptly instead
					// of at the ceiling. It also means
					// events ARE getting through, so the
					// traffic gauge is stamped for the
					// partial commit.
					if newState.PullCursor >
						state.PullCursor {

						deferFailCount = 0

						markIngressEvent()
					}

					state = newState
					ackDirty = false
					redrive.deferredSeq = deferral.eventSeq

					a.sleepDeferralBackoff(
						ctx, &deferFailCount,
					)

					continue
				}

				// A permanent inbound version mismatch is
				// terminal: stop the loop WITHOUT advancing the
				// cursor so the offending envelope is preserved
				// and never acknowledged, matching the legacy
				// dispatch path below. The production store is
				// transactional, so this is the path a real
				// daemon takes.
				if a.checkPermanentStatus(ctx, foldErr) {
					return
				}

				a.log.WarnS(ctx,
					"Transactional dispatch failed",
					foldErr,
					slog.Uint64(
						"cursor", state.PullCursor,
					))

				a.sleepBackoff(ctx, &failCount)

				continue
			}

			// The commit covered the dispatch watermark and any
			// pending ack advance together.
			state = newState
			ackDirty = false
			failCount = 0
			deferFailCount = 0
			redrive.deferredSeq = 0

			a.clearDispatchDeferral(ctx, &episode)
			markIngressEvent()

			continue
		}

		// Step 3 (legacy path): dispatch the batch. On partial
		// failure, the committed cursor reflects only the
		// successfully dispatched portion.
		committedCursor, dispatchErr := a.dispatchBatch(
			ctx, envelopes, nextCursor,
		)
		if dispatchErr != nil {
			// A full target mailbox is backpressure rather than a
			// dispatch failure, and it gets its own throttled log
			// instead of one line per re-pull. The partial advance
			// below is the same in either case: the cursor stops at
			// the undelivered envelope.
			//
			// This path needs no deferredSeq clamp. It dispatches
			// the batch strictly in order and stops at the
			// deferral, so nothing past the undelivered envelope is
			// ever touched to begin with.
			deferred := a.noteDispatchDeferral(
				ctx, dispatchErr, &episode,
			) != nil

			// A permanent inbound version mismatch is terminal:
			// stop the loop WITHOUT advancing the cursor so the
			// offending envelope is preserved for a future
			// compatible restart, and never acknowledged.
			if !deferred && a.checkPermanentStatus(
				ctx, dispatchErr,
			) {
				return
			}

			if !deferred {
				a.log.WarnS(ctx, "Dispatch failed",
					dispatchErr,
					slog.Uint64(
						"committed_to", committedCursor,
					),
				)
			}

			// Even on partial failure, advance state past the
			// last committed envelope so we don't re-dispatch
			// it. dispatchBatch returns the inclusive event_seq
			// of the last successfully dispatched envelope, so
			// we add 1 to get the exclusive next-pull position,
			// consistent with batchNextCursor on the success
			// path.
			nextCursor := committedCursor + 1
			progressed := committedCursor > 0 &&
				nextCursor > state.PullCursor
			if progressed {
				state.AdvanceDispatch(nextCursor)
				state.PullCursor = nextCursor

				if cpErr := a.saveCheckpoint(
					ctx, state,
				); cpErr != nil {

					a.log.WarnS(ctx,
						"Failed to save checkpoint "+
							"after partial dispatch",
						cpErr)
				}
			}

			// Backpressure gets the short redrive schedule, and a
			// redrive that delivered part of the backlog resets it;
			// a real dispatch failure keeps the transport schedule.
			// The reasoning is the same as on the folded path
			// above.
			if deferred {
				if progressed {
					deferFailCount = 0

					markIngressEvent()
				}

				a.sleepDeferralBackoff(ctx, &deferFailCount)

				continue
			}

			a.sleepBackoff(ctx, &failCount)

			continue
		}

		// Step 4: Full batch dispatched successfully. Advance state
		// and persist checkpoint.
		state.AdvanceDispatch(committedCursor)
		state.PullCursor = committedCursor

		if err := a.saveCheckpoint(ctx, state); err != nil {
			a.log.WarnS(
				ctx,
				"Failed to save checkpoint after dispatch",
				err,
			)

			a.sleepBackoff(ctx, &failCount)

			continue
		}

		failCount = 0
		deferFailCount = 0

		a.clearDispatchDeferral(ctx, &episode)
		markIngressEvent()
	}
}

// ackPhase acks any pending dispatches before the next pull so the remote
// mailbox can garbage-collect committed envelopes. It mutates state, ackDirty,
// and failCount in place and returns two loop-control booleans, (exit, retry):
// exit is true when the loop must stop (local shutdown or a permanent version
// error), and retry is true when the caller should back off and continue. On
// the transactional path the advanced watermark is left dirty for the next
// dispatch checkpoint (or idle flush) to persist; the legacy path checkpoints
// inline.
func (a *ServerConnectionActor) ackPhase(ctx context.Context, state *AckState,
	ackDirty *bool, failCount *int, txOK bool) (bool, bool) {

	if !state.NeedsAck() {
		return false, false
	}

	if err := a.ackRemote(ctx, state.AckTarget); err != nil {
		if isIngressShutdownErr(ctx, err) {
			a.logIngressExit(ctx)

			return true, false
		}

		// A permanent version error is terminal: stop the loop rather
		// than retrying forever.
		if a.checkPermanentStatus(ctx, err) {
			return true, false
		}

		a.log.WarnS(ctx, "AckUpTo failed, retrying",
			err,
			slog.Uint64("ack_target", state.AckTarget),
		)

		a.sleepBackoff(ctx, failCount)

		return false, true
	}

	state.AdvanceAck()

	// On the transactional path the advanced watermark is persisted by the
	// next dispatch checkpoint (or the idle flush); losing it to a crash
	// only costs one redundant idempotent AckUpTo on restart. The legacy
	// path keeps the immediate checkpoint.
	if txOK {
		*ackDirty = true
		*failCount = 0

		return false, false
	}

	if err := a.saveCheckpoint(ctx, *state); err != nil {
		a.log.WarnS(
			ctx, "Failed to save checkpoint after ack", err,
		)

		// Don't reset failCount — if the checkpoint store is
		// persistently down, we want backoff to apply on subsequent
		// iterations rather than spinning at full speed.
		a.sleepBackoff(ctx, failCount)

		return false, true
	}

	*failCount = 0

	return false, false
}

// pullPhase pulls the next batch of envelopes from the remote mailbox and
// absorbs the two outcomes that are not a batch to dispatch: a failed pull and
// an empty long-poll. It mutates state, ackDirty and failCount in place and
// returns (envelopes, nextCursor, exit, retry) on the same convention as
// ackPhase — exit is true when the loop must stop (local shutdown or a
// permanent version error), and retry is true when the caller should continue
// without dispatching. Both booleans are false only when envelopes holds a
// non-empty batch, and any backoff a retry needs has already been slept here.
func (a *ServerConnectionActor) pullPhase(ctx context.Context, state *AckState,
	ackDirty *bool, failCount *int) ([]*mailboxpb.Envelope, uint64, bool,
	bool) {

	envelopes, nextCursor, err := a.pullBatch(ctx, state.PullCursor)
	if err != nil {
		if isIngressShutdownErr(ctx, err) {
			a.logIngressExit(ctx)

			return nil, 0, true, false
		}

		// A permanent version error is terminal: stop the loop rather
		// than retrying forever.
		if a.checkPermanentStatus(ctx, err) {
			return nil, 0, true, false
		}

		a.log.WarnS(ctx, "Pull failed, retrying",
			err,
			slog.Uint64("cursor", state.PullCursor),
		)

		a.sleepBackoff(ctx, failCount)

		return nil, 0, false, true
	}

	// The pull returned, so the one goroutine that consumes the remote
	// mailbox is still running its loop. Stamping here rather than after
	// dispatch is what lets a staleness alert separate a parked ingress
	// goroutine from a client that simply has no traffic: an idle client
	// keeps this gauge fresh at the long-poll cadence, and a parked one
	// stops updating it immediately.
	markIngressPoll()

	if len(envelopes) == 0 {
		// Long-poll returned empty. Flush a dirty ack watermark while
		// the connection is idle so a restart does not re-ack forever.
		if *ackDirty {
			if err := a.saveCheckpoint(ctx, *state); err != nil {
				// Back off on a failing checkpoint store rather
				// than retrying at the bare long-poll cadence,
				// mirroring the ack-path policy in ackPhase.
				// ackDirty stays set so the next attempt
				// re-flushes.
				a.log.WarnS(ctx,
					"Failed to flush ack checkpoint "+
						"while idle", err)

				a.sleepBackoff(ctx, failCount)

				return nil, 0, false, true
			}

			*ackDirty = false
		}

		// Reset fail count and loop again immediately — the long-poll
		// timeout already provides the delay.
		*failCount = 0

		return nil, 0, false, true
	}

	a.log.TraceS(
		ctx, "Pulled envelopes",
		slog.Int("count", len(envelopes)),
		slog.Uint64("cursor", state.PullCursor),
		slog.Uint64("next_cursor", nextCursor),
	)

	return envelopes, nextCursor, false, false
}

// logIngressExit emits the common ingress shutdown log line.
func (a *ServerConnectionActor) logIngressExit(ctx context.Context) {
	a.log.InfoS(ctx, "Ingress loop exiting",
		slog.String("mailbox_id", a.cfg.LocalMailboxID),
	)
}

// isIngressShutdownErr reports whether err is an expected result of shutting
// down the ingress loop. Only local loop-context cancellation is terminal; a
// remote transport cancellation can be transient and must stay retryable.
func isIngressShutdownErr(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}

	return ctx.Err() != nil
}

// pullBatch calls Edge.Pull and returns the envelopes and next cursor.
func (a *ServerConnectionActor) pullBatch(ctx context.Context, cursor uint64) (
	[]*mailboxpb.Envelope, uint64, error) {

	waitMs := uint32(a.cfg.PullWaitTimeout.Milliseconds())

	resp, err := a.cfg.Edge.Pull(ctx, &mailboxpb.PullRequest{
		MailboxId:     a.cfg.LocalMailboxID,
		MaxEnvelopes:  a.cfg.PullMaxEnvelopes,
		WaitTimeoutMs: waitMs,
		Cursor:        cursor,
	})
	if sErr := edgeResponseError("Pull", resp, err); sErr != nil {
		return nil, 0, sErr
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
func (a *ServerConnectionActor) dispatchBatch(ctx context.Context,
	envelopes []*mailboxpb.Envelope, batchNextCursor uint64) (uint64,
	error) {

	// Track the cursor of the last successfully dispatched envelope.
	// Start with the current pull cursor as the base.
	lastCommitted := uint64(0)

	for _, env := range envelopes {
		// Validate the envelope's version pair against the runtime
		// binding before delivering it to any waiter or dispatcher. A
		// mismatch is a permanent compatibility failure: stop the batch
		// without advancing the ack cursor so the envelope is preserved
		// for a future compatible restart, and never acknowledge or
		// dispatch it.
		if err := a.validateInboundEnvelope(env); err != nil {
			return lastCommitted, err
		}

		if env.Rpc == nil {
			a.log.WarnS(
				ctx,
				"Skipping envelope without RPC metadata",
				nil,
				slog.Uint64("event_seq", env.EventSeq),
			)

			continue
		}

		// A marked route's dispatcher answers the operator over the
		// network, and the hoist gate has already pulled every
		// KIND_REQUEST on such a route out of the fold. Anything else
		// arriving on one is mislabeled by the sender, and the mux
		// bridge does not look at the kind: it would serve the
		// envelope as a request anyway and put that round trip back
		// under the write transaction, which is the exact stall the
		// split exists to remove. Skip it the way the table skips any
		// other envelope it cannot route, so the batch still makes
		// progress. Returning an error instead would be worse than
		// the stall: a dispatch failure is not permanent, so the loop
		// would back off and re-pull the same envelope forever.
		if a.resolvesToNonTxDispatcher(env) {
			a.log.WarnS(
				ctx,
				"Skipping non-request envelope on a "+
					"non-transactional route",
				nil,
				slog.String("service", env.Rpc.Service),
				slog.String("method", env.Rpc.Method),
				slog.Int("kind", int(env.Rpc.Kind)),
				slog.Uint64("event_seq", env.EventSeq),
			)

			continue
		}

		switch env.Rpc.Kind {
		case mailboxpb.RpcMeta_KIND_RESPONSE:
			// Prefer unary waiters for low-latency RPC
			// callers. When no in-memory waiter is registered,
			// fall back to the durable dispatch table so
			// actor-driven unary flows can treat the response
			// like any other ingress event.
			corrID := CorrelationID(env.Rpc.CorrelationId)
			if corrID == "" {
				a.log.WarnS(ctx,
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

			svcMethod := mailboxrpc.ServiceMethod{
				Service: env.Rpc.Service,
				Method:  env.Rpc.Method,
			}
			dispatcher, ok := a.cfg.Dispatchers[svcMethod]
			if !ok {
				a.log.WarnS(ctx,
					"Failed to deliver response "+
						"envelope",
					nil,
					slog.String(
						"delivery_result",
						fmt.Sprintf("%d", delivery),
					),
					slog.String("service", env.Rpc.Service),
					slog.String("method", env.Rpc.Method),
					slog.String(
						"correlation_id",
						string(corrID),
					),
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
				a.log.WarnS(ctx,
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
			a.log.WarnS(
				ctx,
				"Skipping envelope with unknown RPC kind",
				nil,
				slog.Int("kind", int(env.Rpc.Kind)),
				slog.Uint64("event_seq", env.EventSeq),
			)

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

	return edgeResponseError("AckUpTo", resp, err)
}

// loadCheckpoint restores the AckState from the checkpoint store on startup.
// Returns a zero-value AckState if no checkpoint exists.
func (a *ServerConnectionActor) loadCheckpoint(ctx context.Context) (AckState,
	error) {

	actorID := a.runtimeID()

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

// runFoldedDispatch runs a pulled batch's two non-transactional kinds of
// delivery BEFORE the write transaction, then folds the durable dispatches and
// the advanced AckState checkpoint into ONE commit.
//
// The first pre-transaction kind is waiter-backed responses. Waiter delivery is
// in-memory and at-most-once, cannot be rolled back, and must never wait in
// the single-writer queue: unary callers sit blocked on these with RPC
// deadlines, so gating them on the writer lock turns write contention into
// payment-wide timeout collapse.
//
// The second is the NonTxRoutes requests. Those dispatchers serve an inbound
// KIND_REQUEST through the local mux and put the KIND_RESPONSE back on the
// wire with Edge.Send, so they block on a network round trip and touch no
// local durable mailbox. Running them under the transaction would pin the
// SQLite global writer lock (production opens with _txlock=immediate) or a
// SERIALIZABLE Postgres snapshot across that round trip, stalling or aborting
// every other writer in the process for as long as the operator takes to
// answer. Nothing they do belongs in the fold, so they are hoisted out of it.
//
// Every remaining dispatcher Tell joins the ambient transaction via the
// context (DurableMailbox.Send flows it into EnqueueMessage), so a batch of k
// durable envelopes costs one commit instead of k+1 and the cursor can never
// run ahead of the enqueues: any failure rolls back both, leaves the returned
// state untouched, and the batch is re-pulled intact.
//
// Ordering is pre-transaction work first, commit second, which is what
// at-least-once requires. A crash between a hoisted send and the commit
// re-pulls the batch and redelivers, and the operator absorbs the duplicate
// KIND_RESPONSE by correlation ID exactly as it does for the legacy
// non-transactional dispatch path. Committing first and sending after would
// invert that into at-most-once: a crash in the window would advance the
// cursor past a request that was never answered, and the caller would only
// ever see its own RPC deadline.
//
// The split-time waiter peek is only a hint: a waiter can vanish (RPC
// deadline cancel or TTL prune) between the peek and the actual delivery
// below. The pre-transaction step therefore delivers to LIVE waiters only
// and folds any straggler whose waiter disappeared back into the durable
// transaction, so a durable response enqueue never commits outside the
// cursor fold even if the peek was stale.
//
// redrive carries what the previous cycle of a backpressure episode already
// did. It matters because the pre-transaction work runs over the WHOLE pulled
// batch: with the cursor stalled at a wedged target, every hoisted request in
// the window would otherwise be served again on every redrive, forever. See
// redriveState.
func (a *ServerConnectionActor) runFoldedDispatch(ctx context.Context,
	txStore actor.TxAwareDeliveryStore, envelopes []*mailboxpb.Envelope,
	nextCursor uint64, state AckState, redrive *redriveState) (AckState,
	error) {

	// Validate the whole pulled batch against the bound version pair up
	// front. Only the durable partition is validated inside dispatchBatch,
	// and by then the pre-transaction steps below have already delivered
	// to waiters and answered inbound requests, neither of which can be
	// taken back. Checking every envelope first means one permanently
	// incompatible envelope anywhere in the batch stops the loop before
	// any of the batch is acted on.
	for _, env := range envelopes {
		if err := a.validateInboundEnvelope(env); err != nil {
			return state, err
		}
	}

	// Validation covers the pulled batch; everything after it works on the
	// clamped one, so a redrive repeats no pre-transaction work it already
	// did.
	envelopes, nextCursor = clampToDeferred(
		envelopes, nextCursor, redrive.deferredSeq,
	)

	responses, nonTx, durables := splitIngressEnvelopes(
		envelopes, a.hasResponseWaiter, a.isNonTxRequest,
	)

	// Deliver the waiter-backed responses to their live waiters outside
	// the transaction. Any whose waiter vanished since the split peek come
	// back as stragglers and fold into the durable batch in event_seq
	// order, so their enqueue commits inside the cursor fold, never ahead
	// of it. This runs before the request dispatch below because it is
	// in-memory and instant, while a request costs a full round trip.
	if stragglers := a.deliverWaiterResponses(
		responses,
	); len(stragglers) > 0 {

		durables = mergeEnvelopesByEventSeq(durables, stragglers)
	}

	// Serve the hoisted inbound requests with no transaction open. A
	// failure here returns before the fold, so the cursor does not move and
	// the batch is re-pulled; the requests already answered are not served
	// again, because the watermark that records them advanced before the
	// failure.
	if err := a.dispatchNonTxRequests(ctx, nonTx, redrive); err != nil {
		return state, err
	}

	// The durable partition's in-memory half (a TryTell into a bounded
	// mailbox) is the one delivery below that a rolled-back transaction
	// does not undo, and the production store replays its body on a
	// retryable error. The record is created out here, OUTSIDE the closure,
	// so a replay can see what the previous attempt already handed over and
	// skip it. Everything else in the closure derives from the caller's
	// state and is safe to redo.
	ctx = withDeliveredOutsideTx(ctx)

	var (
		newState AckState
		deferral *deferredDispatchError
	)
	err := txStore.ExecTx(ctx, false, func(txCtx context.Context,
		store actor.DeliveryStore) error {

		// Derive both outputs from the caller's state on every attempt,
		// so a store that runs the closure more than once cannot fold a
		// previous attempt's advance into this one.
		newState = state
		deferral = nil

		cursor := nextCursor
		if len(durables) > 0 {
			_, dispatchErr := a.dispatchBatch(
				txCtx, durables, nextCursor,
			)

			// A full target mailbox is backpressure, not a failed
			// batch: the envelope is intact on the remote mailbox
			// and the loop re-pulls it. Commit up to the
			// undelivered envelope and stop there, because acking
			// past an event that never reached its actor is how
			// backpressure would turn into a lost round event.
			//
			// Nothing to adjust when the whole partition went out:
			// the cursor already covers the batch.
			switch {
			case errors.As(dispatchErr, &deferral):
				cursor = deferredCursor(
					deferral.eventSeq, state.PullCursor,
				)

			case dispatchErr != nil:
				return dispatchErr
			}
		}

		newState.AdvanceDispatch(cursor)
		newState.PullCursor = cursor

		return a.saveCheckpointTo(txCtx, store, newState)
	})
	if err != nil {
		return state, err
	}

	// The prefix and the watermark committed together; the deferral is
	// reported so the loop backs off instead of pulling straight into the
	// same full mailbox.
	if deferral != nil {
		return newState, deferral
	}

	return newState, nil
}

// deferredCursor returns the cursor to commit when a full mailbox stopped a
// batch partway. deferredSeq is the undelivered envelope's own event_seq, and
// that is exactly where the exclusive cursor belongs: everything ahead of it in
// the batch has been fully handled by the time the deferral is raised — waiter
// responses delivered and hoisted requests served before the transaction
// opened, the durable prefix enqueued inside it, unroutable envelopes
// skip-warned — while the deferred envelope itself has not been delivered and
// must be re-pulled. Committing here therefore acks everything before it and
// nothing at or after it, and the next pull starts on the envelope that has to
// be retried.
//
// Stopping one past the last DELIVERED envelope instead would be safe but not
// sufficient: any hoisted request sitting between it and the deferred envelope
// would come back in every redrive's pull window and be served again on each
// one.
//
// The cursor never goes backwards, which keeps a re-pull of already-committed
// envelopes from rewinding it. A zero deferredSeq is impossible — the mailbox
// assigns event_seq from 1, and zero is the never-acked cursor sentinel — but
// it is treated as "do not move" rather than trusted, because reading it as a
// cursor is the one arithmetic here that could ack an undelivered envelope.
func deferredCursor(deferredSeq, pullCursor uint64) uint64 {
	if deferredSeq == 0 || deferredSeq <= pullCursor {
		return pullCursor
	}

	return deferredSeq
}

// clampToDeferred restricts a redriven batch to the envelopes at or before the
// event_seq that a full mailbox turned away last cycle, returning the batch to
// process and the exclusive cursor that covers it.
//
// The clamp is what keeps a redrive from repeating work. The pre-transaction
// steps — waiter delivery and the hoisted request round trips — run over the
// whole batch before the transaction opens, but the cursor stops at the
// deferred envelope, so everything behind it stays inside the pull window for
// as long as the target stays wedged. Without the clamp each of those envelopes
// is re-handled once per backoff cycle, indefinitely: for a hoisted request
// that is a duplicate local serve plus a duplicate response sent to the
// operator, and the routes waiting to be hoisted next are state-changing ones
// where a duplicate is not free.
//
// A batch with nothing at or before deferredSeq is returned whole. That means
// the envelope the last cycle could not deliver is no longer on the mailbox, so
// there is nothing left to protect, and clamping to an empty batch would
// instead let the caller advance the cursor over envelopes it never dispatched.
func clampToDeferred(envelopes []*mailboxpb.Envelope, nextCursor,
	deferredSeq uint64) ([]*mailboxpb.Envelope, uint64) {

	if deferredSeq == 0 {
		return envelopes, nextCursor
	}

	clamped := make([]*mailboxpb.Envelope, 0, len(envelopes))
	for _, env := range envelopes {
		if env.EventSeq <= deferredSeq {
			clamped = append(clamped, env)
		}
	}

	if len(clamped) == 0 || len(clamped) == len(envelopes) {
		return envelopes, nextCursor
	}

	return clamped, deferredSeq + 1
}

// splitIngressEnvelopes partitions a pulled batch into the three ways an
// envelope can be delivered: in-memory waiter responses, hoisted inbound
// requests, and durable dispatches folded into the write transaction. The
// first two run before the transaction opens; only the third belongs in it.
//
// A KIND_RESPONSE only takes the waiter path when an active in-memory waiter
// is registered for its correlation ID, as reported by hasWaiter: those
// callers sit blocked on an RPC deadline and must never queue behind the
// database writer lock. A KIND_RESPONSE with no live waiter would otherwise
// fall through to the durable dispatch table; folding it into the transaction
// alongside requests and events keeps event_seq order on the target actor lane
// and ties its enqueue to the cursor commit, so a rolled-back batch never
// re-delivers it.
//
// An envelope takes the hoisted request path only when isNonTx accepts it,
// which requires both an explicitly marked route and a KIND_REQUEST, so a
// durable enqueue can never leave the fold by accident. Everything else
// (unmarked requests, events, and malformed or correlation-less envelopes,
// which the dispatch loop skip-warns) folds into the transaction.
//
// The three returned slices are, in order, the waiter-backed responses, the
// hoisted requests, and the durable partition.
func splitIngressEnvelopes(envelopes []*mailboxpb.Envelope,
	hasWaiter func(CorrelationID) bool,
	isNonTx func(*mailboxpb.Envelope) bool) ([]*mailboxpb.Envelope,
	[]*mailboxpb.Envelope, []*mailboxpb.Envelope) {

	var responses, nonTx, durables []*mailboxpb.Envelope
	for _, env := range envelopes {
		isResponse := env.Rpc != nil &&
			env.Rpc.Kind == mailboxpb.RpcMeta_KIND_RESPONSE

		// Route a response to the fast pre-transaction path only when a
		// live waiter is registered for its correlation ID; otherwise
		// it folds into the durable transaction with the rest of the
		// batch.
		corrID := CorrelationID("")
		if isResponse {
			corrID = CorrelationID(env.Rpc.CorrelationId)
		}
		if isResponse && corrID != "" && hasWaiter(corrID) {
			responses = append(responses, env)

			continue
		}

		if isNonTx != nil && isNonTx(env) {
			nonTx = append(nonTx, env)

			continue
		}

		durables = append(durables, env)
	}

	return responses, nonTx, durables
}

// isNonTxRequest reports whether an envelope must be dispatched outside the
// folded write transaction. Three conditions all have to hold, and each one
// rules out a different way of hoisting durable work by mistake. The envelope
// must be a KIND_REQUEST, because a KIND_EVENT or the no-waiter KIND_RESPONSE
// fallback resolves to a durable actor Tell that has to commit with the
// cursor. Its route must be listed in NonTxRoutes, because the wiring layer is
// the only place that knows an EnvelopeDispatcher closure terminates in
// Edge.Send rather than in a mailbox enqueue. And a dispatcher must actually
// be registered for the route, so the hoisted path never has to reproduce
// dispatchBatch's skip-warn for an unroutable envelope.
//
// NOTE: hoisting moves the send ahead of the commit, so a crash in between
// leaves the cursor unmoved and the request is served a second time on the
// re-pull. That is free for a read, which is all that is hoisted today, and
// the operator demultiplexes the duplicate response by correlation ID. It
// stops being free for a route that changes state: anything on the money path,
// such as WalletService.SignVTXO or RoundService.SubmitNonces, needs its own
// idempotency before it is added to NonTxRoutes, because correlation-ID dedup
// on the operator does not make a second signature harmless.
//
// That exposure is bounded, and keeping it bounded is load-bearing. A crash or
// a failed fold can repeat a serve; a stalled cursor cannot. Backpressure
// re-pulls the same window every backoff cycle, so without redriveState's
// watermark and clamp one wedged local actor would turn a single queued request
// into one serve per cycle, indefinitely.
func (a *ServerConnectionActor) isNonTxRequest(
	env *mailboxpb.Envelope,
) bool {

	if env.Rpc == nil || env.Rpc.Kind != mailboxpb.RpcMeta_KIND_REQUEST {
		return false
	}

	key := mailboxrpc.ServiceMethod{
		Service: env.Rpc.Service,
		Method:  env.Rpc.Method,
	}

	if _, ok := a.cfg.NonTxRoutes[key]; !ok {
		return false
	}

	_, ok := a.cfg.Dispatchers[key]

	return ok
}

// isNonTxRoute reports whether an envelope's route is marked in NonTxRoutes,
// ignoring its kind. isNonTxRequest is the gate that decides what gets
// hoisted, and it is deliberately stricter. This is the weaker question
// dispatchBatch needs: not "may this be hoisted" but "is this route's
// dispatcher one that answers over the network", which is what makes running
// it inside the write transaction unacceptable no matter what kind the sender
// stamped on the envelope.
func (a *ServerConnectionActor) isNonTxRoute(env *mailboxpb.Envelope) bool {
	if env.Rpc == nil {
		return false
	}

	key := mailboxrpc.ServiceMethod{
		Service: env.Rpc.Service,
		Method:  env.Rpc.Method,
	}

	_, ok := a.cfg.NonTxRoutes[key]

	return ok
}

// resolvesToNonTxDispatcher reports whether an envelope would reach a marked
// route's dispatcher from inside dispatchBatch, which is the one thing the
// fold must not let happen: that dispatcher answers the operator over the
// network, and dispatchBatch runs with the write transaction open.
//
// A KIND_REQUEST is excluded because it is the kind the mark is about. On the
// folded path isNonTxRequest has already hoisted it out; on the legacy path
// there is no transaction to protect, so it dispatches normally. Everything
// else on a marked route is mislabeled by the sender, and the mux bridge does
// not check the kind, so it would be served as a request anyway.
//
// A KIND_RESPONSE with a live waiter is excluded too, because it never reaches
// the dispatch table at all: dispatchBatch hands it to the in-memory waiter and
// breaks. Only the no-waiter fallback resolves to a dispatcher. The folded path
// never presents such an envelope here, since splitIngressEnvelopes peels
// waiter-backed responses off before the fold, but the legacy path does, and
// skipping one there would silently drop a response a caller is blocked on.
// Marked routes carry inbound requests today and waiters belong to outbound
// RPCs, so the two never collide; this keeps that from being load-bearing.
func (a *ServerConnectionActor) resolvesToNonTxDispatcher(
	env *mailboxpb.Envelope) bool {

	if env.Rpc == nil || env.Rpc.Kind == mailboxpb.RpcMeta_KIND_REQUEST {
		return false
	}

	if !a.isNonTxRoute(env) {
		return false
	}

	if env.Rpc.Kind != mailboxpb.RpcMeta_KIND_RESPONSE {
		return true
	}

	corrID := CorrelationID(env.Rpc.CorrelationId)

	return corrID == "" || !a.hasResponseWaiter(corrID)
}

// dispatchNonTxRequests serves the hoisted inbound requests with no write
// transaction open, in event_seq order. Each dispatcher runs the local handler
// and sends the KIND_RESPONSE back over the edge, so this is where the network
// round trip that used to sit inside the fold now happens.
//
// A failure stops the batch and surfaces to the caller, which leaves the cursor
// where it was and re-pulls. Requests this loop already answered are not served
// again on that re-pull: redrive.servedNonTxSeq remembers how far the serving
// got, which is what keeps a stalled cursor from turning one queued request
// into one operator round trip per backoff cycle. The watermark is per ingress
// goroutine, so a restart still redelivers — that is the same at-least-once
// exposure the legacy dispatch path has always had, and the operator
// demultiplexes the duplicate response by correlation ID.
func (a *ServerConnectionActor) dispatchNonTxRequests(ctx context.Context,
	envelopes []*mailboxpb.Envelope, redrive *redriveState) error {

	for _, env := range envelopes {
		// Already answered by an earlier cycle of this episode. The
		// cursor has not passed it yet, so the mailbox keeps handing it
		// back; serving it again would send the operator a second
		// response to a request it has already had answered.
		if env.EventSeq != 0 &&
			env.EventSeq <= redrive.servedNonTxSeq {

			continue
		}

		key := mailboxrpc.ServiceMethod{
			Service: env.Rpc.Service,
			Method:  env.Rpc.Method,
		}

		// isNonTxRequest already proved the lookup succeeds, so a miss
		// here would mean the dispatch table mutated mid-batch. Skip
		// rather than fail, matching how dispatchBatch treats an
		// unroutable envelope, but say so: the cursor is about to
		// advance past a request nobody answered, and that is the one
		// way this path can silently drop instead of redeliver.
		dispatcher, ok := a.cfg.Dispatchers[key]
		if !ok {
			a.log.WarnS(ctx, "No dispatcher for hoisted request",
				nil,
				slog.String("service", env.Rpc.Service),
				slog.String("method", env.Rpc.Method),
				slog.Uint64("event_seq", env.EventSeq),
			)

			continue
		}

		if err := dispatcher(ctx, env); err != nil {
			return err
		}

		// Only a serve that returned records the watermark: a failed
		// one may not have reached the operator, so it has to be
		// retried rather than suppressed.
		if env.EventSeq > redrive.servedNonTxSeq {
			redrive.servedNonTxSeq = env.EventSeq
		}
	}

	return nil
}

// deliverWaiterResponses delivers each split-time waiter-backed response to
// its live in-memory waiter, outside the dispatch transaction. It returns the
// stragglers: responses whose waiter vanished (RPC deadline cancel or TTL
// prune) between the split peek and this delivery. Those must NOT be durably
// dispatched here — that would commit a durable enqueue ahead of the cursor
// fold — so the caller folds them into the transactional durable batch
// instead. A miss may have buffered an early response copy, which is dropped
// so the durable fold remains the single source of truth.
func (a *ServerConnectionActor) deliverWaiterResponses(
	responses []*mailboxpb.Envelope) []*mailboxpb.Envelope {

	var stragglers []*mailboxpb.Envelope
	for _, env := range responses {
		corrID := CorrelationID(env.Rpc.CorrelationId)

		// A correlation-less response cannot match a waiter; defer it
		// to the durable fold like any other non-waiter envelope.
		if corrID == "" {
			stragglers = append(stragglers, env)

			continue
		}

		delivery := a.deliverResponse(corrID, env)
		if delivery == mailboxconn.DeliveryWaiter {
			continue
		}

		// The waiter disappeared after the split peek. Drop any
		// buffered copy and defer the envelope into the durable
		// transaction.
		a.removePendingResponse(corrID)
		stragglers = append(stragglers, env)
	}

	return stragglers
}

// mergeEnvelopesByEventSeq merges two event_seq-ascending envelope slices
// into one ascending slice. Both the durable partition and the straggler set
// derive from a single ordered pass over the pulled batch, so each input is
// already sorted; the merge preserves per-lane FIFO order when stragglers
// fold back into the durable batch.
func mergeEnvelopesByEventSeq(
	a, b []*mailboxpb.Envelope) []*mailboxpb.Envelope {

	merged := make([]*mailboxpb.Envelope, 0, len(a)+len(b))

	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].EventSeq <= b[j].EventSeq {
			merged = append(merged, a[i])
			i++
		} else {
			merged = append(merged, b[j])
			j++
		}
	}

	merged = append(merged, a[i:]...)
	merged = append(merged, b[j:]...)

	return merged
}

// saveCheckpoint persists the AckState to the checkpoint store.
func (a *ServerConnectionActor) saveCheckpoint(
	ctx context.Context, state AckState,
) error {

	return a.saveCheckpointTo(ctx, a.cfg.Store, state)
}

// saveCheckpointTo persists the AckState through the given store, which may
// be a transaction-scoped store so the checkpoint joins an ambient dispatch
// transaction instead of paying its own commit.
func (a *ServerConnectionActor) saveCheckpointTo(ctx context.Context,
	store actor.DeliveryStore, state AckState) error {

	var buf bytes.Buffer
	if err := state.Encode(&buf); err != nil {
		return err
	}

	actorID := a.runtimeID()

	return store.SaveCheckpoint(ctx, actor.CheckpointParams{
		ActorID:   actorID,
		StateType: ackStateType,
		StateData: buf.Bytes(),
	})
}

// sleepBackoff sleeps for an exponential backoff duration with jitter,
// respecting context cancellation. The fail count is incremented on entry
// and used to calculate the delay. The actual backoff arithmetic lives in
// mailboxpull.Sleep so the SDK pull loop and this loop share the same
// schedule.
func (a *ServerConnectionActor) sleepBackoff(ctx context.Context,
	failCount *int) {

	mailboxpull.Sleep(ctx, a.backoffConfig(), failCount)
}

// backoffConfig snapshots the actor's backoff knobs into the shared
// mailboxpull config shape.
func (a *ServerConnectionActor) backoffConfig() mailboxpull.BackoffConfig {
	return mailboxpull.BackoffConfig{
		BaseDelay: a.cfg.RetryBaseDelay,
		MaxDelay:  a.cfg.RetryMaxDelay,
	}
}

// maxDeferralDelay caps the redrive delay during a backpressure episode. It is
// deliberately far below RetryMaxDelay, because the two delays are waiting for
// different things. A transport retry waits for a remote endpoint, where a long
// ceiling is politeness. A backpressure redrive waits for a local actor to
// drain one mailbox turn, and the redrive is the only thing that moves the
// client's inbound backlog: at the transport ceiling a recovered target drains
// at one pull window per 30s, which is slower than the round client's own 60s
// registration and 90s reconcile deadlines and would fail rounds that had
// already put forfeit signatures on the wire.
//
// Half a second is chosen to be longer than any single mailbox turn a healthy
// actor takes, so a briefly busy target is not hammered, and short enough that
// a backlog of several pull windows still drains in seconds. The configured
// RetryMaxDelay still applies when it is SHORTER, so a deployment (or a test)
// that wants a tighter schedule keeps it.
const maxDeferralDelay = 500 * time.Millisecond

// sleepDeferralBackoff sleeps before redriving a deferred dispatch, on the
// backpressure schedule rather than the transport one.
func (a *ServerConnectionActor) sleepDeferralBackoff(ctx context.Context,
	failCount *int) {

	cfg := a.backoffConfig()
	if cfg.MaxDelay <= 0 || cfg.MaxDelay > maxDeferralDelay {
		cfg.MaxDelay = maxDeferralDelay
	}

	// A base delay above the ceiling would make the very first redrive wait
	// longer than the ceiling allows, since the schedule starts at the
	// base.
	if cfg.BaseDelay > cfg.MaxDelay {
		cfg.BaseDelay = cfg.MaxDelay
	}

	mailboxpull.Sleep(ctx, cfg, failCount)
}
