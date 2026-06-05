package oor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	clientdb "github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/ledger"
	libtypes "github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// chainHashOf converts a SessionID into a chainhash.Hash for the registry key.
func chainHashOf(sessionID SessionID) chainhash.Hash {
	return chainhash.Hash(sessionID)
}

// handleGetState returns the current FSM state as a read-only probe.
func (b *sessionBehavior) handleGetState() fn.Result[ActorResp] {
	if b.fsm == nil {
		return fn.Err[ActorResp](fmt.Errorf("session not loaded"))
	}

	state, err := b.fsm.CurrentState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&GetStateResponse{State: state})
}

// handleStartTransfer admits a new outgoing session: it builds the
// deterministic submit package, installs the FSM, stages one
// spending-reservation row per input, and drains the initial outbox (inline Ark
// signing, then the submit transport is collected for commit enqueue).
func (b *sessionBehavior) handleStartTransfer(ctx context.Context,
	req *StartTransferRequest) fn.Result[ActorResp] {

	// Idempotent admission: a duplicate StartTransfer for an
	// already-installed session returns the existing id.
	if b.loaded && b.fsm != nil {
		return fn.Ok[ActorResp](&StartTransferResponse{
			SessionID: b.sessionID,
			Existing:  true,
		})
	}

	session, outbox, err := NewSessionWithIdempotencyKey(
		ctx, req.Policy, req.Inputs, req.Recipients, req.IdempotencyKey,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	b.fsm = session.FSM
	b.sessionID = session.ID
	b.direction = clientdb.OORSessionDirectionOutgoing
	b.loaded = true

	b.logger(ctx).InfoS(ctx, "OOR session created",
		slog.String("session_id", session.ID.String()),
		slog.Int("num_outbox", len(outbox)),
	)

	// Stage one durable spending-reservation row per input. The write joins
	// the commit transaction so a row exists IFF the session is
	// checkpointed.
	inputs := req.Inputs
	b.commitWork = append(b.commitWork,
		func(txCtx context.Context, _ oorTx) error {
			return b.recordReservations(txCtx, inputs)
		},
	)

	if err := b.driveOutbox(ctx, outbox); err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&StartTransferResponse{SessionID: session.ID})
}

// handleDriveEvent feeds a follow-up event (typically a server response) into
// the session FSM and drains the resulting outbox. On a finalize acceptance it
// stages the finalized-package persistence and queues the ledger emission.
func (b *sessionBehavior) handleDriveEvent(ctx context.Context,
	req *DriveEventRequest) fn.Result[ActorResp] {

	if req == nil || req.Event == nil {
		return fn.Err[ActorResp](fmt.Errorf("event must be provided"))
	}
	if b.fsm == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("unknown session: %s", b.sessionID),
		)
	}

	// An inbound SubmitAcceptedEvent may be missing the ArkPSBT (operators
	// that do not echo co_signed_ark_psbt back); enrich it from the current
	// state before the FSM transition rejects the nil packet.
	if err := b.enrichSubmitAccepted(req.Event); err != nil {
		return fn.Err[ActorResp](err)
	}

	// Capture the finalize-state context before applying so the finalized
	// package and the ledger emission can be staged.
	finalizeState, err := b.captureFinalizeState(req.Event)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	outbox, err := b.apply(ctx, req.Event)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	if err := b.driveOutbox(ctx, outbox); err != nil {
		return fn.Err[ActorResp](err)
	}

	if finalizeState != nil {
		state := finalizeState
		b.commitWork = append(b.commitWork,
			func(txCtx context.Context, _ oorTx) error {
				return b.persistOutgoingPackage(txCtx, state)
			},
		)
		b.postCommit = append(b.postCommit, func(ctx context.Context) {
			b.emitVTXOSent(ctx, state)
		})
	}

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// enrichSubmitAccepted populates a SubmitAcceptedEvent's missing ArkPSBT from
// the current session state. The canonical ArkPSBT lives in the
// AwaitingSubmitAccepted state, set when the client built and sent the submit
// package; the server response proto is not required to echo it back, so the
// dispatch adapter may construct the event with only the session id and the
// co-signed checkpoints.
func (b *sessionBehavior) enrichSubmitAccepted(event Event) error {
	submitAccepted, ok := event.(*SubmitAcceptedEvent)
	if !ok || submitAccepted.ArkPSBT != nil {
		return nil
	}

	state, err := b.fsm.CurrentState()
	if err != nil {
		return fmt.Errorf("get current state for ark psbt "+
			"enrichment: %w", err)
	}

	awaitingSubmit, ok := state.(*AwaitingSubmitAccepted)
	if !ok {
		return fmt.Errorf("expected AwaitingSubmitAccepted state for "+
			"ark psbt enrichment, got %T", state)
	}

	submitAccepted.ArkPSBT = awaitingSubmit.ArkPSBT

	return nil
}

// captureFinalizeState snapshots the AwaitingFinalizeAccepted state before a
// FinalizeAcceptedEvent advances past it, so the finalized package can be
// persisted in the commit transaction.
func (b *sessionBehavior) captureFinalizeState(event Event) (
	*AwaitingFinalizeAccepted, error) {

	if b.cfg.PackageStore == nil {
		return nil, nil
	}
	if _, ok := event.(*FinalizeAcceptedEvent); !ok {
		return nil, nil
	}

	current, err := b.fsm.CurrentState()
	if err != nil {
		return nil, err
	}

	finalizeState, ok := current.(*AwaitingFinalizeAccepted)
	if !ok {
		return nil, nil
	}

	return finalizeState, nil
}

// recordReservations writes one spending-reservation row per outgoing input.
func (b *sessionBehavior) recordReservations(ctx context.Context,
	inputs []TransferInput) error {

	if b.cfg.ReservationStore == nil {
		return nil
	}

	ownerID := chainHashOf(b.sessionID)
	for _, op := range InputOutpoints(inputs) {
		err := b.cfg.ReservationStore.UpsertReservation(
			ctx, op, ReservationOwnerKindOOROutgoing, ownerID,
		)
		if err != nil {
			return fmt.Errorf("record spending reservation for "+
				"session %s outpoint %s: %w", b.sessionID, op,
				err)
		}
	}

	return nil
}

// completeSpend marks the session's consumed VTXO inputs as spent. It filters
// to locally-known outpoints, then routes completion through the VTXO manager
// (SpendCompleter, which joins this turn's transaction) or, as a fallback,
// writes the spent status directly to the VTXO store.
func (b *sessionBehavior) completeSpend(ctx context.Context,
	outpoints []wire.OutPoint) error {

	if len(outpoints) == 0 {
		return nil
	}

	// Filter to outpoints the local VTXO store knows about; non-local
	// inputs (e.g. the counterparty's) require no spend completion here.
	known := outpoints
	if b.cfg.VTXOStore != nil {
		known = make([]wire.OutPoint, 0, len(outpoints))
		for _, op := range outpoints {
			_, err := b.cfg.VTXOStore.GetVTXO(ctx, op)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("load local vtxo %s: %w", op,
					err)
			}

			known = append(known, op)
		}
	}

	if len(known) == 0 {
		return nil
	}

	if b.cfg.SpendCompleter != nil {
		return b.cfg.SpendCompleter(ctx, known)
	}

	if b.cfg.VTXOStore == nil {
		return fmt.Errorf("vtxo store must be provided")
	}

	for _, op := range known {
		err := b.cfg.VTXOStore.UpdateVTXOStatus(
			ctx, op, vtxo.VTXOStatusSpent,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// persistOutgoingPackage writes the finalized outgoing package and its input
// bindings. Non-local input bindings are skipped.
func (b *sessionBehavior) persistOutgoingPackage(ctx context.Context,
	state *AwaitingFinalizeAccepted) error {

	if b.cfg.PackageStore == nil || state == nil {
		return nil
	}

	sessionHash := chainHashOf(b.sessionID)

	err := b.cfg.PackageStore.UpsertPackage(
		ctx, PackageDirectionOutgoing, sessionHash, state.ArkPSBT,
		state.FinalCheckpointPSBTs,
	)
	if err != nil {
		return err
	}

	outpoints := InputOutpoints(state.TransferInputs)
	for i := range outpoints {
		err := b.cfg.PackageStore.UpsertBinding(
			ctx, outpoints[i], sessionHash, uint32(i),
			PackageLinkKindConsumedInput,
		)
		if errors.Is(err, libtypes.ErrOORBindingOutpointNotFound) {
			continue
		}
		if err != nil {
			return err
		}
	}

	return nil
}

// emitVTXOSent posts the outgoing-transfer ledger entry after commit.
func (b *sessionBehavior) emitVTXOSent(ctx context.Context,
	state *AwaitingFinalizeAccepted) {

	b.cfg.LedgerSink.WhenSome(func(sink ledger.Sink) {
		if state == nil || len(state.TransferInputs) == 0 {
			return
		}

		var total int64
		for i := range state.TransferInputs {
			total += int64(state.TransferInputs[i].VTXO.Amount)
		}
		if total <= 0 {
			return
		}

		msg := &ledger.VTXOSentMsg{
			SessionID: b.sessionID,
			AmountSat: total,
		}
		if err := sink.Tell(ctx, msg); err != nil {
			b.logger(ctx).WarnS(
				ctx,
				"Failed to emit VTXOSentMsg to ledger",
				err,
				slog.String("session_id", b.sessionID.String()),
				slog.Int64("amount_sat", total),
			)
		}
	})
}

// buildTransportMessage wraps an outbox transport event into the serverconn
// message that is durably delivered to the operator.
func (b *sessionBehavior) buildTransportMessage(ctx context.Context,
	event OutboxEvent) (serverconn.ServerConnMsg, error) {

	serverMsg, ok := event.(serverconn.ServerMessage)
	if !ok {
		return nil, fmt.Errorf("transport event %T does not implement "+
			"ServerMessage", event)
	}

	sm := serverMsg.ServiceMethod()

	return &serverconn.SendClientEventRequest{
		Message: serverMsg,
		Service: sm.Service,
		Method:  sm.Method,
	}, nil
}

// enqueueTransport writes one cross-actor transport message to the durable
// outbox so the outbox publisher delivers it to serverconn after commit.
func (b *sessionBehavior) enqueueTransport(ctx context.Context, tx oorTx,
	msg serverconn.ServerConnMsg) error {

	if b.cfg.ServerConn == nil {
		return fmt.Errorf("serverconn ref must be provided")
	}

	payload, err := serverConnOutboxCodec.Encode(msg)
	if err != nil {
		return fmt.Errorf("encode serverconn message: %w", err)
	}

	return tx.store.EnqueueOutbox(ctx, actor.OutboxParams{
		ID:            uuid.Must(uuid.NewV7()).String(),
		SourceActorID: b.actorID,
		TargetActorID: b.cfg.ServerConn.ID(),
		MessageType:   msg.MessageType(),
		Payload:       payload,
	})
}

// handleResolveIncomingTransfer admits/continues an incoming receive session.
//
// TODO(oor): port the incoming receive flow (resolve -> metadata -> materialize
// -> ack) to the direct per-session model. Tracked as part of the OOR
// per-session refactor.
func (b *sessionBehavior) handleResolveIncomingTransfer(_ context.Context,
	_ *ResolveIncomingTransferRequest) fn.Result[ActorResp] {

	return fn.Err[ActorResp](
		fmt.Errorf("incoming receive not yet wired on the " +
			"per-session actor"),
	)
}

// handleResumeSession re-drives the outbox implied by the current state after a
// retry timer fires.
//
// TODO(oor): port resume/retry to the direct per-session model.
func (b *sessionBehavior) handleResumeSession(_ context.Context,
	_ *ResumeSessionRequest) fn.Result[ActorResp] {

	return fn.Err[ActorResp](
		fmt.Errorf("resume not yet wired on the per-session actor"),
	)
}
