package serverconn

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/serverconn/mailboxpull"
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
		envelopes, nextCursor, err := a.pullBatch(
			ctx, state.PullCursor,
		)
		if err != nil {
			if isIngressShutdownErr(ctx, err) {
				a.logIngressExit(ctx)

				return
			}

			// A permanent version error is terminal: stop the loop
			// rather than retrying forever.
			if a.checkPermanentStatus(ctx, err) {
				return
			}

			a.log.WarnS(ctx, "Pull failed, retrying",
				err,
				slog.Uint64("cursor", state.PullCursor),
			)

			a.sleepBackoff(ctx, &failCount)

			continue
		}

		if len(envelopes) == 0 {
			// Long-poll returned empty. Flush a dirty ack
			// watermark while the connection is idle so a
			// restart does not re-ack forever.
			if ackDirty {
				if err := a.saveCheckpoint(
					ctx, state,
				); err != nil {

					// Back off on a failing checkpoint
					// store rather than retrying at the
					// bare long-poll cadence, mirroring the
					// ack-path policy above. ackDirty stays
					// set so the next attempt re-flushes.
					a.log.WarnS(ctx,
						"Failed to flush ack "+
							"checkpoint while idle",
						err)

					a.sleepBackoff(ctx, &failCount)

					continue
				}

				ackDirty = false
			}

			// Reset fail count and loop again immediately — the
			// long-poll timeout already provides the delay.
			failCount = 0

			continue
		}

		a.log.TraceS(
			ctx, "Pulled envelopes",
			slog.Int("count", len(envelopes)),
			slog.Uint64("cursor", state.PullCursor),
			slog.Uint64("next_cursor", nextCursor),
		)

		// Step 3 (transactional path): deliver in-memory responses
		// outside the transaction, then fold the durable dispatches
		// and the advanced watermark into one commit.
		if txOK {
			newState, foldErr := a.runFoldedDispatch(
				ctx, txStore, envelopes, nextCursor, state,
			)
			if foldErr != nil {
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

			continue
		}

		// Step 3 (legacy path): dispatch the batch. On partial
		// failure, the committed cursor reflects only the
		// successfully dispatched portion.
		committedCursor, dispatchErr := a.dispatchBatch(
			ctx, envelopes, nextCursor,
		)
		if dispatchErr != nil {
			// A permanent inbound version mismatch is terminal:
			// stop the loop WITHOUT advancing the cursor so the
			// offending envelope is preserved for a future
			// compatible restart, and never acknowledged.
			if a.checkPermanentStatus(ctx, dispatchErr) {
				return
			}

			a.log.WarnS(ctx, "Dispatch failed",
				dispatchErr,
				slog.Uint64("committed_to", committedCursor),
			)

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

					a.log.WarnS(ctx,
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
			a.log.WarnS(
				ctx,
				"Failed to save checkpoint after dispatch",
				err,
			)

			a.sleepBackoff(ctx, &failCount)

			continue
		}

		failCount = 0
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

// runFoldedDispatch delivers a pulled batch's waiter-backed response
// envelopes BEFORE the write transaction, then folds the durable dispatches
// and the advanced AckState checkpoint into ONE commit. Waiter delivery is
// in-memory and at-most-once, cannot be rolled back, and must never wait in
// the single-writer queue: unary callers sit blocked on these with RPC
// deadlines, so gating them on the writer lock turns write contention into
// payment-wide timeout collapse. Every durable dispatcher Tell joins the
// ambient transaction via the context (DurableMailbox.Send flows it into
// EnqueueMessage), so a batch of k durable envelopes costs one commit
// instead of k+1 and the cursor can never run ahead of the enqueues: any
// failure rolls back both, leaves the returned state untouched, and the
// batch is re-pulled intact.
//
// The split-time waiter peek is only a hint: a waiter can vanish (RPC
// deadline cancel or TTL prune) between the peek and the actual delivery
// below. The pre-transaction step therefore delivers to LIVE waiters only
// and folds any straggler whose waiter disappeared back into the durable
// transaction, so a durable response enqueue never commits outside the
// cursor fold even if the peek was stale.
func (a *ServerConnectionActor) runFoldedDispatch(ctx context.Context,
	txStore actor.TxAwareDeliveryStore, envelopes []*mailboxpb.Envelope,
	nextCursor uint64, state AckState) (AckState, error) {

	responses, durables := splitIngressEnvelopes(
		envelopes, a.hasResponseWaiter,
	)

	// Waiter-backed responses deliver in-memory BEFORE the transaction and
	// so bypass dispatchBatch, which is where the durable partition is
	// validated. Validate them against the bound version pair here so every
	// inbound envelope is checked before dispatch, just like the legacy
	// path. A mismatch is permanent and surfaces to the caller, which
	// drives the terminal incompatibility transition.
	for _, resp := range responses {
		if err := a.validateInboundEnvelope(resp); err != nil {
			return state, err
		}
	}

	// Deliver the waiter-backed responses to their live waiters outside
	// the transaction. Any whose waiter vanished since the split peek come
	// back as stragglers and fold into the durable batch in event_seq
	// order, so their enqueue commits inside the cursor fold, never ahead
	// of it.
	if stragglers := a.deliverWaiterResponses(
		responses,
	); len(stragglers) > 0 {

		durables = mergeEnvelopesByEventSeq(durables, stragglers)
	}

	newState := state
	err := txStore.ExecTx(ctx, false, func(txCtx context.Context,
		store actor.DeliveryStore) error {

		if len(durables) > 0 {
			_, dispatchErr := a.dispatchBatch(
				txCtx, durables, nextCursor,
			)
			if dispatchErr != nil {
				return dispatchErr
			}
		}

		newState.AdvanceDispatch(nextCursor)
		newState.PullCursor = nextCursor

		return a.saveCheckpointTo(txCtx, store, newState)
	})
	if err != nil {
		return state, err
	}

	return newState, nil
}

// splitIngressEnvelopes partitions a pulled batch into in-memory response
// envelopes and durable dispatch envelopes. A KIND_RESPONSE only takes the
// pre-transaction path when an active in-memory waiter is registered for its
// correlation ID, as reported by hasWaiter: those callers sit blocked on an
// RPC deadline and must never queue behind the database writer lock. A
// KIND_RESPONSE with no live waiter would otherwise fall through to the durable
// dispatch table; folding it into the transaction alongside requests and events
// keeps event_seq order on the target actor lane and ties its enqueue to the
// cursor commit, so a rolled-back batch never re-delivers it. Everything else
// (requests, events, and malformed or correlation-less envelopes, which the
// dispatch loop skip-warns) folds into the transaction too.
func splitIngressEnvelopes(envelopes []*mailboxpb.Envelope,
	hasWaiter func(CorrelationID) bool) ([]*mailboxpb.Envelope,
	[]*mailboxpb.Envelope) {

	var responses, durables []*mailboxpb.Envelope
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
		} else {
			durables = append(durables, env)
		}
	}

	return responses, durables
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

	actorID := DurableActorID(a.cfg.LocalMailboxID)

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
