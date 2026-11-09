package round

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/timeout"
	"github.com/lightninglabs/wavelength/wallet"
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

// checkBoardingInputsAvailable prevents a retired checkpoint's deposits from
// being registered again while its replacement still owns them in memory.
func (a *RoundClientActor) checkBoardingInputsAvailable(ctx context.Context,
	intents []BoardingIntent) error {

	owned := make(map[wire.OutPoint]struct{})
	scanCtx, cancel := fsmScanContext(ctx)
	defer cancel()
	for _, active := range a.rounds {
		state, err := fsmState(scanCtx, active.FSM)
		if err != nil {
			return fmt.Errorf("read boarding ownership: %w", err)
		}
		for _, intent := range stateBoarding(state) {
			owned[intent.Outpoint] = struct{}{}
		}
	}
	var duplicate *wire.OutPoint
	var unowned []wire.OutPoint
	for _, intent := range intents {
		if _, exists := owned[intent.Outpoint]; exists {
			op := intent.Outpoint
			duplicate = &op
		} else {
			unowned = append(unowned, intent.Outpoint)
		}
	}
	if duplicate == nil {
		return nil
	}
	// A wallet trigger marks its whole package shipped before the reply
	// arrives here. Release the new inputs from the rejected package, while
	// retaining every input already owned by a tracked round.
	if len(unowned) > 0 && a.cfg.WalletActor != nil {
		if err := a.cfg.WalletActor.Tell(
			ctx, &wallet.ReleaseBoardingInFlightRequest{
				Outpoints: unowned,
			},
		); err != nil {
			return fmt.Errorf("release rejected boarding "+
				"package: %w", err)
		}
	}

	return fmt.Errorf("%w: %s", ErrBoardingInFlight, duplicate)
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
		fresh := a.rounds[RoundKeyStr(nextID.KeyString())]
		if fresh != nil &&
			fresh.RetryAttempt == failed.RetryAttempt {
			return a.deliverReplacementQuote(ctx, nextID)
		}

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
	if a.env.AdmissionTimeout > 0 &&
		(old.env == nil || old.env.admission == nil) {

		old.PendingReplacement = nil

		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("replacement has no original admission " +
				"budget"),
		)
	}
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
	fresh, err := a.createRoundWithState(ctx, &IntentSentState{
		AdmittedRoundID: nextID, Intents: intents,
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
	fresh.AssetDeposit = old.AssetDeposit
	if err := a.cfg.RoundStore.CloseAdmissionDeadline(
		ctx, oldID,
	); err != nil {

		a.log.WarnS(ctx, "Failed to close abandoned admission deadline",
			err,
		)
	}
	old.FSM.Stop()
	delete(a.rounds, RoundKeyStr(oldID.KeyString()))
	delete(a.commitmentTxIndex, old.TxID)
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

	return a.deliverReplacementQuote(ctx, nextID)
}

// roundReplacementRetryMsg retains one transient local recovery operation.
// It is not a durable mailbox payload; persisted checkpoints keep their normal
// status reconciliation if the process exits before the reminder runs.
type roundReplacementRetryMsg struct {
	actor.BaseMessage
	Offer        *BoardingFailed
	QuoteRoundID RoundID
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
		old.PendingReplacement = nil
		a.log.WarnS(ctx, "Unable to retain round replacement cleanup",
			err,
		)
	}

	return fn.Err[actormsg.RoundActorResp](cause)
}

// deliverReplacementQuote retains emitted effects until each is delivered.
// Replaying the quote itself after the FSM advances cannot re-emit an accept.
func (a *RoundClientActor) deliverReplacementQuote(ctx context.Context,
	id RoundID) fn.Result[actormsg.RoundActorResp] {

	fresh := a.rounds[RoundKeyStr(id.KeyString())]
	if fresh == nil {
		return fn.Ok[actormsg.RoundActorResp](nil)
	}
	if quote := a.pendingQuotes[id]; quote != nil {
		future := fresh.FSM.AskEvent(ctx, quote)
		result := future.Await(ctx) //actor:allow-await leaf quote FSM
		events, err := result.Unpack()
		if err != nil {
			return a.retryReplacementQuote(ctx, fresh, err)
		}
		fresh.ReplacementQuoteOutbox = append(
			fresh.ReplacementQuoteOutbox, events...,
		)
		delete(a.pendingQuotes, id)
	}
	for len(fresh.ReplacementQuoteOutbox) > 0 {
		if err := a.processOutbox(
			ctx, fresh.ReplacementQuoteOutbox[:1],
		); err != nil {
			return a.retryReplacementQuote(ctx, fresh, err)
		}
		fresh.ReplacementQuoteOutbox = fresh.ReplacementQuoteOutbox[1:]
	}
	a.releaseBoardingOnFailure(ctx, fresh)

	return fn.Ok[actormsg.RoundActorResp](&ServerMessageResponse{
		Success: true,
	})
}

// retryReplacementQuote bounds local retries without requiring another offer.
func (a *RoundClientActor) retryReplacementQuote(ctx context.Context,
	fresh *RoundFSM, cause error) fn.Result[actormsg.RoundActorResp] {

	if fresh.ReplacementQuoteFailures < 5 && a.cfg.TimeoutActor != nil {
		fresh.ReplacementQuoteFailures++
		callback := timeout.MapTimeoutExpired(a.cfg.SelfRef,
			func(timeout.ExpiredMsg) actormsg.RoundReceivable {
				return &roundReplacementRetryMsg{
					QuoteRoundID: fresh.RoundID,
				}
			},
		)
		err := a.cfg.TimeoutActor.Tell(a.lifecycleCtx(ctx),
			&timeout.ScheduleTimeoutRequest{
				ID: timeout.ID(
					"replacement-quote:" +
						fresh.RoundID.String(),
				),
				Duration: 2 * time.Second, Callback: callback,
			},
		)
		if err == nil {
			return fn.Err[actormsg.RoundActorResp](cause)
		}
	}
	delete(a.pendingQuotes, fresh.RoundID)
	fresh.ReplacementQuoteOutbox = nil

	return a.handleServerMessage(ctx, &ServerMessageNotification{
		Message: &BoardingFailed{
			RoundID:     fn.Some(fresh.RoundID),
			Error:       cause,
			Reason:      "replacement quote delivery failed",
			Recoverable: true,
		},
	})
}
