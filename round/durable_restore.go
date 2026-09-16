package round

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/timeout"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// installActorSnapshot installs validated state without replaying signer calls.
// The caller owns the actor turn and must reconstruct process-local watches
// through its durable outbox before acknowledging the restart message.
func (a *RoundClientActor) installActorSnapshot(ctx context.Context,
	snapshot *clientActorSnapshot) {

	for _, previous := range a.rounds {
		previous.FSM.Stop()
	}
	a.rounds = make(map[RoundKeyStr]*RoundFSM, len(snapshot.rounds))
	for key, restored := range snapshot.rounds {
		round := restored.round
		round.coldSigning = restored.lostSignerSessions
		a.installRoundState(ctx, round, restored.state)
		a.rounds[key] = round
	}
	a.pendingQuotes = snapshot.pendingQuotes
	a.pendingTimers = snapshot.pendingTimers
	a.commitmentTxIndex = snapshot.commitmentTxIndex
}

// restoreRuntimeEffects reconstructs process-local watches through the outbox.
// Existing timers retain their original absolute deadlines across restarts.
func (a *RoundClientActor) restoreRuntimeEffects(ctx context.Context,
	turn *durableClientTurn) error {

	for _, timer := range a.pendingTimers {
		if err := turn.captureFrozen(timer); err != nil {
			return err
		}
	}
	for _, round := range a.rounds {
		// Legacy checkpoints may predate the persistent outbox. Replay
		// only already-created signatures, under the ordinary service
		// authorization guard, so upgrading cannot strand a submission.
		if err := a.replayCheckpointedServerMessages(
			ctx, round,
		); err != nil {
			return err
		}
		state, err := round.FSM.CurrentStateWithContext(ctx)
		if err != nil {
			return err
		}
		typed, ok := state.(ClientState)
		if !ok {
			return fmt.Errorf("unexpected restored state %T", state)
		}
		values, err := clientStateSnapshot(typed)
		if err != nil {
			return err
		}
		if round.coldSigning ||
			values.kind == snapshotInputSigSentState {

			probe := statusReconcileProbeOutbox(
				round.RoundID, round.env, 0,
				values.intents.Service,
			)
			id := makeTimeoutID(
				RoundKeyStr(
					round.RoundID.KeyString(),
				),
				TimeoutPhaseStatusReconcile,
			)
			if a.pendingTimers[id] != nil ||
				round.env.StatusReconcileTimeout <= 0 {

				probe = probe[:1]
			}
			if err := a.processOutbox(ctx, probe); err != nil {
				return err
			}
		}
		if round.TxID == (chainhash.Hash{}) ||
			round.CommitmentTx.IsNone() {

			continue
		}
		packet := round.CommitmentTx.UnwrapOr(nil)
		if packet == nil || packet.UnsignedTx == nil {
			return fmt.Errorf("restored commitment is missing")
		}
		request := &RegisterConfirmationRequest{
			CallerID: fmt.Sprintf("commitment-tx-%s", round.TxID),
			Txid:     &round.TxID,
			PkScript: confirmationWatchScript(
				packet.UnsignedTx, values.trees,
			),
			TargetConfs: round.env.OperatorTerms.
				VTXOTargetConfirmations(),
			HeightHint: round.env.StartHeight,
		}
		if err := turn.capture(ctx, request); err != nil {
			return err
		}
	}

	return nil
}

// installRoundState starts an inline runner without invoking entry actions.
func (a *RoundClientActor) installRoundState(ctx context.Context,
	round *RoundFSM,
	state protofsm.State[ClientEvent, ClientOutMsg, *ClientEnvironment]) {

	if round.FSM != nil {
		round.FSM.Stop()
	}
	machine := protofsm.NewInlineStateMachine(ClientStateMachineCfg{
		Logger: a.log, InitialState: state, Env: round.env,
	})
	round.FSM = &machine
	a.startRoundFSM(ctx, round.FSM)
}

// processColdRoundEvent keeps the original signing state and ownership until
// the authenticated operator reports that the round can no longer complete.
// Local cancellation, elapsed time, and another signing request cannot
// establish that fact. The operator-report trust boundary matches normal
// reconciliation.
func (a *RoundClientActor) processColdRoundEvent(ctx context.Context,
	round *RoundFSM, event ClientEvent) error {

	state, err := round.FSM.CurrentStateWithContext(ctx)
	if err != nil {
		return err
	}
	clientState, ok := state.(ClientState)
	if !ok {
		return fmt.Errorf("unexpected cold round state %T", state)
	}
	values, err := clientStateSnapshot(clientState)
	if err != nil {
		return err
	}
	switch message := event.(type) {
	case *BoardingFailed:
		return fmt.Errorf("round signing session lost; awaiting " +
			"authoritative reconciliation")

	case *StatusReconcileTimedOut:
		return a.processOutbox(
			ctx, statusReconcileProbeOutbox(
				round.RoundID, round.env, 0,
				values.intents.Service,
			),
		)

	case *RoundStatusReported:
		if message.RoundID != round.RoundID ||
			message.Status != roundStatusDead {
			return nil
		}
		service := values.intents.Service
		if service != nil && (message.Operation == nil ||
			message.Operation.Phase != operationFailed ||
			!bytes.Equal(
				message.Operation.OperationId,
				service.OperationID[:],
			)) {
			return nil
		}

		transition := failWithNotification(
			"round signing session lost and operator reports dead",
			nil, true, fn.Some(round.RoundID),
		)
		transition, err = releaseForfeitsOnFailure(
			transition, nil, fn.Some(round.RoundID),
			values.intents.Forfeits,
		)
		if err != nil {
			return err
		}
		transition = appendReconcileDisarm(
			transition, round.RoundID, round.env,
		)
		a.installRoundState(ctx, round, transition.NextState)
		round.coldSigning = false

		return a.processOutbox(
			ctx, transition.NewEvents.UnwrapOr(
				ClientEmittedEvent{},
			).Outbox,
		)

	default:
		return nil
	}
}

// importLegacyRounds upgrades the previous relational checkpoint format only
// when no native actor checkpoint exists. Validation finishes before replacing
// live maps; the restart turn commits the imported snapshot and its effects.
func (a *RoundClientActor) importLegacyRounds(ctx context.Context) error {
	rounds, err := a.cfg.RoundStore.ListActiveRounds(ctx)
	if err != nil {
		return fmt.Errorf("load legacy round checkpoints: %w", err)
	}
	snapshot := &clientActorSnapshot{
		rounds:            make(map[RoundKeyStr]*restoredClientRound),
		pendingQuotes:     make(map[RoundID]*JoinRoundQuoteReceived),
		pendingTimers:     make(map[timeout.ID]*durableClientEffect),
		commitmentTxIndex: make(map[chainhash.Hash]RoundKeyStr),
	}
	for _, legacy := range rounds {
		restored, err := a.createRoundFSMFromDB(ctx, legacy.RoundID)
		if err != nil {
			return err
		}
		encoded, err := encodeDurableRound(ctx, restored)
		restored.FSM.Stop()
		if err != nil {
			return err
		}
		decoded, err := decodeDurableRound(encoded, a.env)
		if err != nil {
			return err
		}
		key := RoundKeyStr(legacy.RoundID.KeyString())
		snapshot.rounds[key] = decoded
		if restored.TxID != (chainhash.Hash{}) {
			snapshot.commitmentTxIndex[restored.TxID] = key
		}
	}
	if err := a.OnStop(ctx); err != nil {
		return err
	}
	a.installActorSnapshot(ctx, snapshot)

	return nil
}
