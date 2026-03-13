package oor

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightningnetwork/lnd/input"
)

// SigningOutboxHandler implements the signing and scheduling outbox requests
// emitted by the OOR FSM.
//
// This handler is intended to be used as the Next delegate inside
// LocalPersistenceOutboxHandler. It handles the following outbox events:
//
//   - RequestArkSignatures: signs Ark PSBT inputs using the client key on
//     each checkpoint output's owner leaf via SignArkPSBT.
//   - RequestCheckpointSignatures: attaches client-side collaborative VTXO
//     spend signatures to each checkpoint PSBT via SignCheckpointPSBTs.
//   - ScheduleRetryRequest: schedules a timer via the timeout actor. When
//     the timer fires, a DriveEventRequest{Event: &RetryDueEvent{}} is
//     delivered back to the OOR actor via the CallbackRef.
//   - IncomingTransferNotification: informational no-op (logging only).
type SigningOutboxHandler struct {
	// Signer signs checkpoint inputs at RequestCheckpointSignatures.
	Signer input.Signer

	// TimeoutActor schedules retry timers. When nil, retry requests
	// return a RetryDueEvent immediately (useful for test harnesses).
	TimeoutActor *timeout.Actor

	// CallbackRef receives timeout expiry notifications transformed into
	// OOR actor messages. This is typically created via
	// actor.NewMapInputRef to convert *timeout.ExpiredMsg into a
	// DriveEventRequest targeting the OOR actor.
	CallbackRef actor.TellOnlyRef[*timeout.ExpiredMsg]
}

// Handle executes one outbox request and returns follow-up FSM events.
func (h *SigningOutboxHandler) Handle(ctx context.Context,
	sessionID SessionID, outbox OutboxEvent) ([]Event, error) {

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		log.DebugS(ctx, "Ark signatures requested",
			slog.String("session_id", sessionID.String()),
			slog.Int("num_inputs",
				len(msg.ArkPSBT.UnsignedTx.TxIn)))

		return h.handleArkSignatures(msg)

	case *RequestCheckpointSignatures:
		log.DebugS(ctx, "Checkpoint signatures requested",
			slog.String("session_id", sessionID.String()),
			slog.Int("num_checkpoints", len(msg.CoSignedCheckpointPSBTs)))

		return h.handleCheckpointSignatures(msg)

	case *ScheduleRetryRequest:
		log.InfoS(ctx, "Scheduling retry",
			slog.String("session_id", sessionID.String()),
			slog.String("reason", msg.Reason),
			slog.Duration("after", msg.After))

		return h.handleScheduleRetry(ctx, sessionID, msg)

	case *IncomingTransferNotification:
		// Informational notification for UI/logging. No FSM
		// follow-up events are required.
		log.InfoS(ctx, "Incoming transfer notification received",
			slog.String("session_id", msg.SessionID.String()),
			slog.Int("num_recipients", len(msg.Recipients)))

		return nil, nil

	default:
		return nil, fmt.Errorf("unhandled outbox event %T", outbox)
	}
}

// handleArkSignatures signs the Ark PSBT inputs using the client key on the
// owner leaf path of each checkpoint output.
func (h *SigningOutboxHandler) handleArkSignatures(
	msg *RequestArkSignatures) ([]Event, error) {

	if h.Signer == nil {
		return nil, fmt.Errorf("signer is required")
	}

	err := SignArkPSBT(
		h.Signer, msg.ArkPSBT, msg.CheckpointPSBTs,
		msg.TransferInputs,
	)
	if err != nil {
		return nil, err
	}

	return []Event{
		&ArkSignedEvent{
			ArkPSBT: msg.ArkPSBT,
		},
	}, nil
}

// handleCheckpointSignatures attaches client-side collaborative VTXO spend
// signatures to each checkpoint PSBT.
func (h *SigningOutboxHandler) handleCheckpointSignatures(
	msg *RequestCheckpointSignatures) ([]Event, error) {

	if h.Signer == nil {
		return nil, fmt.Errorf("signer is required")
	}

	err := SignCheckpointPSBTs(
		h.Signer, msg.TransferInputs, msg.CoSignedCheckpointPSBTs,
	)
	if err != nil {
		return nil, err
	}

	return []Event{
		&CheckpointsSignedEvent{
			FinalCheckpointPSBTs: msg.CoSignedCheckpointPSBTs,
		},
	}, nil
}

// handleScheduleRetry schedules a retry timer via the timeout actor. The
// session ID is encoded as the timeout ID so the expiry callback can
// reconstruct a DriveEventRequest targeting the correct session. When no
// timeout actor is configured, a RetryDueEvent is returned immediately for
// backward compatibility with test harnesses.
func (h *SigningOutboxHandler) handleScheduleRetry(ctx context.Context,
	sessionID SessionID, msg *ScheduleRetryRequest) ([]Event, error) {

	if h.TimeoutActor == nil {
		// No timeout actor configured: emit RetryDueEvent
		// immediately so tests don't need timer infrastructure.
		return []Event{&RetryDueEvent{}}, nil
	}

	if h.CallbackRef == nil {
		return nil, fmt.Errorf("timeout callback ref not wired")
	}

	// Use the session ID hex string as the timeout ID. When the timer
	// fires, the callback ref's map function parses this back into a
	// SessionID for the DriveEventRequest.
	timeoutID := timeout.ID(sessionID.String())

	result := h.TimeoutActor.Receive(ctx, &timeout.ScheduleTimeoutRequest{
		ID:       timeoutID,
		Duration: msg.After,
		Callback: h.CallbackRef,
	})
	if result.IsErr() {
		return nil, fmt.Errorf("schedule retry timeout: %w",
			result.Err())
	}

	// The timeout actor will deliver RetryDueEvent asynchronously
	// via the callback ref when the timer fires.
	return nil, nil
}

var _ OutboxHandler = (*SigningOutboxHandler)(nil)
