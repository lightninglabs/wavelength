package round

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/timeout"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// retryMaterial retains the original intent while discarding all transcripts.
func retryMaterial(
	state protofsm.State[ClientEvent, ClientOutMsg, *ClientEnvironment],
	roundID RoundID) (Intents, map[SignerKey]*tree.SignerSession, bool,
	bool) {

	switch s := state.(type) {
	case *RoundJoinedState:
		return s.Intents, nil, false, s.RoundID == roundID

	case *CommitmentTxReceivedState:
		return s.Intents, nil, false, s.RoundID == roundID

	case *CommitmentTxValidatedState:
		return s.Intents, nil, false, s.RoundID == roundID

	case *NoncesSentState:
		return s.Intents, s.Musig2Sessions, false, s.RoundID == roundID

	case *NoncesAggregatedState:
		return s.Intents, s.Musig2Sessions, false, s.RoundID == roundID

	case *PartialSigsSentState:
		return s.Intents, s.Musig2Sessions, false, s.RoundID == roundID

	case *ForfeitSignaturesCollectingState:
		return s.Intents, nil, false, s.RoundID == roundID

	case *InputSigSentState:
		return s.Intents, nil, true, s.RoundID == roundID

	default:
		return Intents{}, nil, false, false
	}
}

// isReplacedRound fences late server events for abandoned attempt identifiers.
func (a *RoundClientActor) isReplacedRound(id RoundID) bool {
	for _, active := range a.rounds {
		for _, old := range active.ReplacedRounds {
			if old == id {
				return true
			}
		}
	}

	return false
}

// replacedRoundStrings carries reservation authority only for an active
// lineage.
func (a *RoundClientActor) replacedRoundStrings(id string) []string {
	fsm := a.rounds[RoundKeyStr(id)]
	if fsm == nil {
		return nil
	}
	result := make([]string, 0, len(fsm.ReplacedRounds))
	for _, old := range fsm.ReplacedRounds {
		result = append(result, old.String())
	}

	return result
}

// handleRoundReplacement accepts an authenticated operator abort under the same
// authority boundary as dead-status reconciliation. A checkpoint retirement
// failure retains a bounded local reminder. Reserved
// forfeits transfer directly when the next connector-bound signature is made.
func (a *RoundClientActor) handleRoundReplacement(ctx context.Context,
	failed *BoardingFailed) fn.Result[actormsg.RoundActorResp] {

	success := fn.Ok[actormsg.RoundActorResp](
		&ServerMessageResponse{
			Success: true,
		},
	)
	oldID := failed.RoundID.UnwrapOr(RoundID{})
	nextID := failed.ReplacementRoundID.UnwrapOr(RoundID{})
	if a.isReplacedRound(oldID) {
		return success
	}
	old := a.rounds[RoundKeyStr(oldID.KeyString())]
	if old == nil || nextID == (RoundID{}) || oldID == nextID ||
		failed.RetryAttempt != old.RetryAttempt+1 ||
		failed.RetryAttempt > 10 {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("invalid round replacement"),
		)
	}
	if _, exists := a.rounds[RoundKeyStr(nextID.KeyString())]; exists {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("replacement round already exists"),
		)
	}
	pending := old.PendingReplacement
	if pending != nil &&
		pending.ReplacementRoundID != failed.ReplacementRoundID {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("conflicting round replacement"),
		)
	}
	state, err := fsmState(ctx, old.FSM)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](err)
	}
	intents, sessions, checkpointed, ok := retryMaterial(state, oldID)
	if !ok {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("round state cannot be replaced"),
		)
	}
	old.PendingReplacement = failed
	if err := cleanupSignerSessions(sessions); err != nil {
		return a.retryRoundReplacement(ctx, old, failed, err)
	}
	if checkpointed {
		if err := a.cfg.RoundStore.FailRound(ctx, oldID); err != nil {
			return a.retryRoundReplacement(
				ctx, old, failed,
				fmt.Errorf("retire abandoned checkpoint: %w",
					err),
			)
		}
	}

	// A replacement has no accepted quote yet. Its original intent is
	// evaluated against the fresh quote before any new tree or input
	// signature is created.
	intents.QuotedLeaveAmounts = nil
	var priorBudget *admissionBudget
	if old.env != nil {
		priorBudget = old.env.admission
	}
	fresh, err := a.createRoundWithState(ctx, &RoundJoinedState{
		RoundID: nextID, Intents: intents,
	}, fn.Some(nextID), priorBudget)
	if err != nil {
		return a.retryRoundReplacement(ctx, old, failed, err)
	}
	fresh.RetryAttempt = failed.RetryAttempt
	fresh.ReplacedRounds = append(
		append(
			[]RoundID(nil), old.ReplacedRounds...,
		),
		oldID,
	)
	fresh.BoardingOutpoints = old.BoardingOutpoints
	if err := a.cfg.RoundStore.CloseAdmissionDeadline(
		ctx, oldID,
	); err != nil {

		a.log.WarnS(ctx, "Failed to close abandoned admission deadline",
			err,
		)
	}
	old.FSM.Stop()
	delete(a.rounds, RoundKeyStr(oldID.KeyString()))
	a.dropStagedLedger(oldID)
	// Cancel old local phase clocks; callbacks already queued are
	// round-fenced.
	for _, phase := range []TimeoutPhase{TimeoutPhaseRegistration,
		TimeoutPhaseForfeitCollection, TimeoutPhaseStatusReconcile,
		TimeoutPhaseAdmission} {
		if a.cfg.TimeoutActor == nil {
			break
		}
		if err := a.processOutbox(ctx, []ClientOutMsg{&CancelTimeoutReq{
			RoundKey: RoundKeyStr(oldID.KeyString()), Phase: phase,
		}}); err != nil {

			a.log.DebugS(
				ctx,
				"Abandoned round timer cancellation failed",
				slog.Any("err", err),
			)
		}
	}
	a.log.InfoS(ctx, "Accepted replacement round",
		slog.String("previous_round_id", oldID.String()),
		slog.String("round_id", nextID.String()),
		slog.Uint64("retry_attempt", uint64(failed.RetryAttempt)),
	)
	if quote := a.pendingQuotes[nextID]; quote != nil {
		if err := a.askEventAndProcessOutbox(
			ctx, fresh, quote,
		); err != nil {

			// Keep the quote buffered when delivery did not finish.
			return fn.Err[actormsg.RoundActorResp](err)
		}
		delete(a.pendingQuotes, nextID)
	}

	return success
}

// roundReplacementRetryMsg retains one transient local recovery operation.
// It is not a durable mailbox payload; persisted checkpoints keep their normal
// status reconciliation if the process exits before the reminder runs.
type roundReplacementRetryMsg struct {
	actor.BaseMessage
	Offer *BoardingFailed
}

// RoundReceivable permits delivery through the round actor's service key.
func (*roundReplacementRetryMsg) RoundReceivable() {}

// MessageType names the bounded local reminder.
func (*roundReplacementRetryMsg) MessageType() string {
	return "roundReplacementRetryMsg"
}

// isAbandonedAttempt fences both completed handoffs and cleanup in progress.
func (a *RoundClientActor) isAbandonedAttempt(id RoundID) bool {
	if a.isReplacedRound(id) {
		return true
	}
	active := a.rounds[RoundKeyStr(id.KeyString())]

	return active != nil && active.PendingReplacement != nil
}

// retryRoundReplacement gives transient cleanup/storage failures five bounded
// reminders. Exhaustion leaves the original checkpoint to status reconciliation
// or its existing admission deadline; neither guesses that signatures are safe.
func (a *RoundClientActor) retryRoundReplacement(ctx context.Context,
	old *RoundFSM, offer *BoardingFailed,
	cause error) fn.Result[actormsg.RoundActorResp] {

	if old.ReplacementFailures >= 5 || a.cfg.TimeoutActor == nil {
		// Restore status reconciliation once local replacement work
		// gives up.
		old.PendingReplacement = nil
		a.log.WarnS(ctx, "Round replacement cleanup exhausted",
			cause,
			slog.String("round_id", old.RoundID.String()),
		)

		return fn.Err[actormsg.RoundActorResp](cause)
	}
	old.ReplacementFailures++
	callback := timeout.MapTimeoutExpired(a.cfg.SelfRef,
		func(timeout.ExpiredMsg) actormsg.RoundReceivable {
			return &roundReplacementRetryMsg{Offer: offer}
		},
	)
	err := a.cfg.TimeoutActor.Tell(a.lifecycleCtx(ctx),
		&timeout.ScheduleTimeoutRequest{
			ID: timeout.ID(
				"round-replacement:" +
					old.RoundID.String(),
			),
			Duration: 2 * time.Second, Callback: callback,
		},
	)
	if err != nil {
		a.log.WarnS(ctx, "Unable to retain round replacement cleanup",
			err,
		)
	}

	return fn.Err[actormsg.RoundActorResp](cause)
}
