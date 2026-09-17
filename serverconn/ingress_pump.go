package serverconn

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/baselib/actor"
)

// IngressPumpResult describes work observed by one mailbox ingress invocation.
// It does not describe actor, egress, quarantine or payment completion.
type IngressPumpResult struct {
	// Batches counts nonempty batches whose dispatch was attempted. A
	// failed or partially committed dispatch also consumes one batch.
	Batches uint32

	// MailboxEmpty means a successful pull observed no remote envelopes
	// after any pending ACK was flushed. Messages may arrive immediately
	// afterwards, and actors or outbound requests may still need work.
	MailboxEmpty bool
}

// PumpIngress processes at most maxBatches nonempty batches under the caller's
// deadline, with no long-poll, backoff sleep or background workers. Both a
// deadline and a positive maxBatches are required. PullMaxEnvelopes must also
// be positive; oversized responses are rejected before any dispatch.
//
// The connector and its dispatch targets must already be configured, with a
// transactional delivery store. This shares foreground ingress's dispatch,
// receipt, quarantine and checkpoint fold. Existing in-memory actor routes
// remain volatile; a committed cursor alone is not durable operation state.
// It stops on the first error or
// backpressure; a later invocation resumes from durable state. Cursors remain
// internal. A nil error with MailboxEmpty false means the batch limit was
// reached, so the host should schedule more work.
//
// Cancellation is cooperative: edge, store and dispatcher implementations must
// honor context. The deadline does not cancel work already durably handed to
// actors, nor does it provide a hard wall-clock bound for uncooperative code.
// This primitive does not run actors, egress or quarantine/receipt maintenance.
// The host must coordinate those before declaring the wallet idle.
func (a *ServerConnectionActor) PumpIngress(ctx context.Context,
	maxBatches uint32) (IngressPumpResult, error) {

	var result IngressPumpResult
	if _, ok := ctx.Deadline(); !ok {
		return result, fmt.Errorf("ingress pump requires a deadline")
	}
	if maxBatches == 0 || a.cfg.PullMaxEnvelopes == 0 {
		return result, fmt.Errorf("ingress pump requires positive " +
			"batch and envelope limits")
	}
	txStore, ok := a.cfg.Store.(actor.TxAwareDeliveryStore)
	if !ok {
		return result, fmt.Errorf("ingress pump requires a " +
			"transactional delivery store")
	}
	pumpCtx, cancel := context.WithCancel(ctx)
	run, err := a.beginIngress(pumpCtx, cancel)
	if err != nil {
		cancel()

		return result, err
	}
	defer a.finishIngress(run)

	result, err = a.pumpIngress(pumpCtx, txStore, maxBatches)
	a.checkPermanentStatus(pumpCtx, err)

	return result, a.ingressError(pumpCtx, err)
}

// pumpIngress owns one checkpoint session. Every ACK is derived from a loaded
// or successfully committed state; an interrupted ACK/checkpoint may be safely
// repeated by the next invocation without repeating the durable dispatch.
func (a *ServerConnectionActor) pumpIngress(ctx context.Context,
	txStore actor.TxAwareDeliveryStore, maxBatches uint32) (
	IngressPumpResult, error) {

	var result IngressPumpResult
	state, err := a.loadCheckpoint(ctx)
	if err != nil {
		return result, fmt.Errorf("load ingress checkpoint: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}
	if a.cfg.AuthSignature != nil || a.cfg.TLSBindSignature != nil {
		if err := a.sendHeartbeatOnce(ctx); err != nil {
			return result, err
		}
	}

	var redrive redriveState
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if state.NeedsAck() {
			if err := a.ackRemote(
				ctx, state.AckTarget,
			); err != nil {
				return result, err
			}
			state.AdvanceAck()
			if err := a.saveCheckpoint(ctx, state); err != nil {
				return result, err
			}
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if result.Batches == maxBatches {
			return result, nil
		}

		envelopes, next, err := a.pullBatchWithWait(
			ctx, state.PullCursor, 0,
		)
		if err != nil {
			return result, err
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if uint64(len(envelopes)) > uint64(a.cfg.PullMaxEnvelopes) {
			return result, fmt.Errorf("ingress batch exceeds " +
				"configured envelope limit")
		}
		markIngressPoll()
		if len(envelopes) == 0 {
			result.MailboxEmpty = true

			return result, nil
		}

		result.Batches++
		newState, err := a.runFoldedDispatch(
			ctx, txStore, envelopes, next, state, &redrive,
		)
		if newState.PullCursor > state.PullCursor {
			markIngressEvent()
		}
		if err != nil {

			// A deferral can commit a prefix. Leave its ACK to the
			// next invocation, which reloads that exact checkpoint.
			return result, err
		}
		state = newState
	}
}
