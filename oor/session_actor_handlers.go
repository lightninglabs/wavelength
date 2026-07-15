package oor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/ledger"
	libtypes "github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/vtxo"
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
		b.envConfig(),
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	b.setFSM(session.FSM)
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
		b.queueVTXOSent(ctx, state)
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

		// The canonical ArkPSBT only exists in AwaitingSubmitAccepted.
		// Any other state means the FSM has already advanced past
		// submit, so this nil-ArkPSBT event is a stale duplicate (an
		// at-least-once operator redelivering its push after a
		// reconnect, with the dedup TTL missed). Leave the event
		// unenriched and let apply() discard it as an unexpected event
		// (a clean no-op ack) rather than erroring, which would Nack
		// and retry to the dead-letter against the deterministic,
		// durable FSM.
		return nil
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
// (SpendCompleter) or, as a fallback, writes the spent status directly to the
// VTXO store. It must be called inline in dispatch with no OOR writer
// transaction held: the SpendCompleter Ask drives a write in the VTXO actor's
// own transaction, which would deadlock against a held OOR writer lock.
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

// releaseSpend returns the session's reserved input VTXOs from SpendingState
// to LiveState after a pre-point-of-no-return terminal failure. Like
// completeSpend it filters to locally-known outpoints (a non-local input
// carries no local reservation to release) and routes the rest through the
// VTXO manager via SpendReleaser. The caller treats this as best-effort: the
// FSM is already terminal Failed, so the startup sweep remains the backstop
// for any reservation this path fails to clear.
func (b *sessionBehavior) releaseSpend(ctx context.Context,
	outpoints []wire.OutPoint) error {

	if len(outpoints) == 0 {
		return nil
	}

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

	if b.cfg.SpendReleaser == nil {
		return fmt.Errorf("no spend releaser configured")
	}

	return b.cfg.SpendReleaser(ctx, known)
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

// queueVTXOSent stages the outgoing-transfer ledger entry for the durable
// outbox enqueue in commitAck, so the accounting message lands atomically
// with the finalized snapshot.
func (b *sessionBehavior) queueVTXOSent(ctx context.Context,
	state *AwaitingFinalizeAccepted) {

	if !b.cfg.LedgerSink.IsSome() {
		return
	}
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

	b.logger(ctx).DebugS(ctx, "Queueing VTXO-sent ledger entry for commit",
		slog.String("session_id", b.sessionID.String()),
		slog.Int64("amount_sat", total),
	)

	b.pendingLedger = append(b.pendingLedger, &ledger.VTXOSentMsg{
		SessionID: b.sessionID,
		AmountSat: total,
	})
}

// buildTransportMessage wraps an outbox transport event into the serverconn
// message that is durably delivered to the operator. The two incoming indexer
// queries map to dedicated requests; every other transport event is a plain
// ServerMessage wrapped into a client-event request.
func (b *sessionBehavior) buildTransportMessage(ctx context.Context,
	event OutboxEvent) (serverconn.ServerConnMsg, error) {

	switch queryReq := event.(type) {
	case *QueryIncomingTransferRequest:
		afterEventID := uint64(0)
		if queryReq.RecipientEventID > 0 {
			afterEventID = queryReq.RecipientEventID - 1
		}

		return &serverconn.SendListOORRecipientEventsByScriptRequest{
			PkScript: append(
				[]byte(nil), queryReq.RecipientPkScript...,
			),
			AfterEventID: afterEventID,
			Limit:        1,
			CorrelationID: IncomingResolveCorrelationID(
				queryReq.SessionID, queryReq.RecipientEventID,
			),
		}, nil

	case *QueryIncomingMetadataRequest:
		recipients := queryReq.Recipients
		filter, ok := b.cfg.IncomingHandler.(incomingMetadataFilter)
		if ok {
			owned, err := filter.FilterIncomingMetadataRecipients(
				ctx, queryReq.Recipients,
			)
			if err != nil {

				// Transient: the filter hit a store/wallet
				// error that may clear on retry, so propagate
				// it plainly to redeliver.
				return nil, fmt.Errorf("filter incoming "+
					"metadata recipients: %w", err)
			}

			recipients = owned
		}

		if len(recipients) == 0 {

			// Deterministic: the operator-supplied recipient set
			// contains nothing this wallet owns. Re-running the
			// identical turn never changes that, so mark it
			// terminal rather than redeliver forever.
			return nil, terminalOutboxErrorf(
				"incoming metadata query contains no " +
					"wallet-owned recipients",
			)
		}

		pkScripts := make([][]byte, 0, len(recipients))
		for i := range recipients {
			pkScripts = append(
				pkScripts,
				append(
					[]byte(nil), recipients[i].PkScript...,
				),
			)
		}

		return &serverconn.SendListVTXOsByScriptsRequest{
			PkScripts: pkScripts,
			Limit:     b.cfg.Limits.MaxVTXOMatches,
			CorrelationID: IncomingMetadataCorrelationID(
				queryReq.SessionID,
			),
		}, nil
	}

	serverMsg, ok := event.(serverconn.ServerMessage)
	if !ok {

		// Deterministic: the FSM emitted a transport event that is not
		// a ServerMessage. This is a fixed property of the event type,
		// so fail terminally instead of redelivering.
		return nil, terminalOutboxErrorf(
			"transport event %T does not implement ServerMessage",
			event,
		)
	}

	sm := serverMsg.ServiceMethod()

	return &serverconn.SendClientEventRequest{
		Message: serverMsg,
		Service: sm.Service,
		Method:  sm.Method,
	}, nil
}

// tellTransport delivers the turn's collected cross-actor transport messages
// (submit / finalize / ack) directly into the serverconn durable actor. Run
// inside the commit transaction: serverconn is a durable actor, so each Tell
// persists the message into its mailbox via the ambient txCtx and the message
// lands IFF the turn commits. The network send runs later on serverconn's own
// egress turn, outside this tx, and is retried by serverconn, so a committed
// OOR turn can never lose its transport obligation and no separate outbox
// publisher hop is needed.
func (b *sessionBehavior) tellTransport(ctx context.Context) error {
	if len(b.pendingTransport) == 0 {
		return nil
	}

	if b.cfg.ServerConn == nil {
		return fmt.Errorf("serverconn ref must be provided")
	}

	for _, msg := range b.pendingTransport {
		if err := b.cfg.ServerConn.Tell(ctx, msg); err != nil {
			return fmt.Errorf("tell serverconn transport: %w", err)
		}
	}

	return nil
}

// tellLedger delivers the turn's staged accounting messages to the durable
// ledger actor. Run inside the commit transaction: the ledger actor's
// DurableMailbox.Send joins the ambient transaction from ctx, so each message
// is persisted into the ledger's mailbox atomically with the snapshot and a
// committed turn can never lose its accounting to a crash.
func (b *sessionBehavior) tellLedger(ctx context.Context) error {
	if len(b.pendingLedger) == 0 {
		return nil
	}

	sink, err := b.cfg.LedgerSink.UnwrapOrErr(
		fmt.Errorf("ledger sink must be provided"),
	)
	if err != nil {
		return err
	}

	for _, msg := range b.pendingLedger {
		if err := sink.Tell(ctx, msg); err != nil {
			return fmt.Errorf("tell ledger: %w", err)
		}
	}

	return nil
}

// handleResolveIncomingTransfer admits a new incoming receive session in the
// ReceiveResolving state and drains its outbox (the phase-1 indexer query that
// resolves the lightweight hint into the full Ark/checkpoint package).
func (b *sessionBehavior) handleResolveIncomingTransfer(ctx context.Context,
	req *ResolveIncomingTransferRequest) fn.Result[ActorResp] {

	if req == nil || len(req.RecipientPkScript) == 0 {
		return fn.Err[ActorResp](
			fmt.Errorf("recipient pk script must be provided"),
		)
	}

	// A duplicate hint for an already-admitted session is a no-op; the
	// resolve query is already in flight or the session has progressed.
	if b.loaded && b.fsm != nil {
		return fn.Ok[ActorResp](&DriveEventResponse{})
	}

	session, err := newReceiveSessionWithState(
		ctx, req.SessionID, &ReceiveResolving{
			SessionID: req.SessionID,
			RecipientPkScript: append(
				[]byte(nil), req.RecipientPkScript...,
			),
			RecipientEventID: req.RecipientEventID,
		}, b.envConfig(),
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	b.setFSM(session.FSM)
	b.sessionID = req.SessionID
	b.direction = clientdb.OORSessionDirectionIncoming
	b.loaded = true

	state, err := b.fsm.CurrentState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	outbox, err := OutboxForIncomingState(state)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	if err := b.driveOutbox(ctx, outbox); err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// handleResumeSession re-drives the outbox implied by the current state after a
// retry timer fires.
func (b *sessionBehavior) handleResumeSession(ctx context.Context,
	req *ResumeSessionRequest) fn.Result[ActorResp] {

	if b.fsm == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("unknown session: %s", b.sessionID),
		)
	}

	state, err := b.fsm.CurrentState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	// A give-up timer expiry drives RetryDueEvent through the FSM so the
	// persisted attempt counter advances and the session fails terminally
	// at the bound, instead of merely re-emitting the query forever. This
	// applies to both wait states that arm a give-up timer:
	// ReceiveResolving (phase-1 hint) and ReceiveNotified (phase-2
	// metadata). A boot restore (FromRetryTimer false) must NOT drive
	// RetryDueEvent: it only re-arms the timer from the persisted count, so
	// repeated restarts cannot burn through the give-up budget faster than
	// the time-based schedule.
	if req.FromRetryTimer && waitsOnGiveUpTimer(state) {
		next, err := b.apply(ctx, &RetryDueEvent{})
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		if err := b.driveOutbox(ctx, next); err != nil {
			return fn.Err[ActorResp](err)
		}

		return fn.Ok[ActorResp](&DriveEventResponse{})
	}

	var outbox []OutboxEvent
	switch b.direction {
	case clientdb.OORSessionDirectionOutgoing:
		outgoing, ok := state.(State)
		if !ok {
			return fn.Err[ActorResp](
				fmt.Errorf("unexpected outgoing state "+
					"type: %T", state),
			)
		}
		outbox, err = OutboxForState(outgoing)

	case clientdb.OORSessionDirectionIncoming:
		outbox, err = resumeOutboxForIncomingState(state)

	default:
		return fn.Err[ActorResp](
			fmt.Errorf("unknown session direction: %d",
				b.direction),
		)
	}
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	if err := b.driveOutbox(ctx, outbox); err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// materializeIncoming runs the incoming VTXO materialization through the shared
// local-persistence handler (whose writes join the commit transaction via ctx),
// then drives the resulting IncomingHandledEvent: it queues the VTXO-manager
// notification for after commit and advances the FSM toward the ack.
func (b *sessionBehavior) materializeIncoming(ctx context.Context,
	msg *MaterializeIncomingVTXOsRequest) error {

	if b.cfg.IncomingHandler == nil {
		return fmt.Errorf("incoming handler must be provided")
	}

	followUps, err := b.cfg.IncomingHandler.Handle(ctx, b.sessionID, msg)
	if err != nil {
		return err
	}

	for _, event := range followUps {
		b.notifyMaterialized(ctx, event)

		next, err := b.apply(ctx, event)
		if err != nil {
			return err
		}

		// Raw drive: this runs inside the commit transaction, so a
		// deterministic error must roll the commit back (retry) rather
		// than fail the FSM mid-commit.
		if err := b.driveOutboxEvents(ctx, next); err != nil {
			return err
		}
	}

	return nil
}

// notifyMaterialized stages the cross-actor notifications for newly
// materialized incoming VTXOs. The ledger receive entries are collected for
// the in-transaction durable outbox enqueue (a committed materialization can
// never lose its accounting). The VTXO manager and fraud-observer Tells stay
// post-commit best-effort: both targets re-derive their state at boot from
// the VTXO rows this turn persists (Manager.Start respawns an actor per live
// VTXO, and initFraudWatcher re-tracks the manager's live set), so a crash
// inside the post-commit window is healed by the next restart.
//
// The notifications run on their own goroutine with a daemon-owned context,
// mirroring notifyTerminal: the turn context is on its way out when the
// materializing turn is also the terminal one (the registry reaps the child
// right after commit), and a notification dropped to that cancellation is NOT
// healed until the next restart. In a long-running daemon that strands the
// materialized VTXO as live-but-actorless, so every coin selection that picks
// it fails with "no actor for outpoint" until the operator restarts. The
// daemon-owned context keeps delivery independent of this child's lifetime,
// and the goroutine keeps a busy VTXO manager mailbox from wedging the turn.
func (b *sessionBehavior) notifyMaterialized(ctx context.Context, event Event) {
	handled, ok := event.(*IncomingHandledEvent)
	if !ok || len(handled.MaterializedVTXOs) == 0 {
		return
	}

	descs := handled.MaterializedVTXOs
	b.queueVTXOsReceived(ctx, descs)

	vtxoManager := b.cfg.VTXOManager
	observer := b.cfg.IncomingVTXOObserver
	log := b.log

	b.postCommit = append(b.postCommit, func(_ context.Context) {
		//nolint:contextcheck // The notification is daemon-owned, not
		// turn-scoped: the materializing turn may also be the terminal
		// one, whose context dies as the registry reaps this child.
		go func() {
			ctx, cancel := context.WithTimeout(
				context.Background(), terminalNotifyTimeout,
			)
			defer cancel()

			if vtxoManager != nil {
				err := vtxoManager.Tell(
					ctx,
					&vtxo.VTXOsMaterializedNotification{
						VTXOs: descs,
					},
				)
				if err != nil {
					log.WarnS(ctx, "Failed to notify "+
						"VTXO manager of "+
						"materialized VTXOs", err)
				}
			}

			if observer != nil {
				if err := observer(ctx, descs); err != nil {
					log.WarnS(ctx, "Incoming VTXO "+
						"observer failed", err)
				}
			}
		}()
	})
}

// queueVTXOsReceived stages one VTXOReceivedMsg per materialized incoming
// VTXO for the durable outbox enqueue in commitAck.
func (b *sessionBehavior) queueVTXOsReceived(ctx context.Context,
	descs []*vtxo.Descriptor) {

	if !b.cfg.LedgerSink.IsSome() {
		return
	}

	b.logger(ctx).DebugS(
		ctx,
		"Queueing VTXO-received ledger entries for commit",
		slog.String("session_id", b.sessionID.String()),
		slog.Int("num_vtxos", len(descs)),
	)

	for _, desc := range descs {
		if desc == nil {
			continue
		}

		b.pendingLedger = append(
			b.pendingLedger, &ledger.VTXOReceivedMsg{
				OutpointHash:  desc.Outpoint.Hash,
				OutpointIndex: desc.Outpoint.Index,
				AmountSat:     int64(desc.Amount),
				Source:        ledger.SourceOOR,
			},
		)
	}
}
