package oor

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/timeout"
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
//     the timer fires, a ResumeSessionRequest is delivered back to the OOR
//     actor via the CallbackRef.
//   - IncomingTransferNotification: informational no-op (logging only).
type SigningOutboxHandler struct {
	// Signer signs checkpoint inputs at RequestCheckpointSignatures.
	Signer input.Signer

	// TimeoutActor schedules retry timers. The handler Tells schedule
	// requests through this ref so all delivery flows through the
	// timeout actor's mailbox; the returned future is not awaited
	// because schedule acks carry no information the caller needs.
	// When nil, retry requests are treated as no-ops and callers must
	// resume explicitly.
	TimeoutActor actor.TellOnlyRef[timeout.Msg]

	// CallbackRef receives timeout expiry notifications transformed into
	// OOR actor messages. This is typically created via
	// actor.NewMapInputRef to convert *timeout.ExpiredMsg into a
	// ResumeSessionRequest targeting the OOR actor.
	CallbackRef actor.TellOnlyRef[*timeout.ExpiredMsg]
}

// Handle executes one outbox request and returns follow-up FSM events.
func (h *SigningOutboxHandler) Handle(ctx context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		numInputs := 0
		if msg.ArkPSBT != nil && msg.ArkPSBT.UnsignedTx != nil {
			numInputs = len(msg.ArkPSBT.UnsignedTx.TxIn)
		}

		logger(ctx).DebugS(ctx, "Ark signatures requested",
			slog.String("session_id", sessionID.String()),
			slog.Int("num_inputs", numInputs),
		)

		return h.handleArkSignatures(msg)

	case *RequestCheckpointSignatures:
		logger(ctx).DebugS(ctx, "Checkpoint signatures requested",
			slog.String("session_id", sessionID.String()),
			slog.Int(
				"num_checkpoints",
				len(msg.CoSignedCheckpointPSBTs),
			),
		)

		return h.handleCheckpointSignatures(ctx, msg)

	case *ScheduleRetryRequest:
		logger(ctx).InfoS(ctx, "Scheduling retry",
			slog.String("session_id", sessionID.String()),
			slog.String("reason", msg.Reason),
			slog.Duration("after", msg.After),
		)

		return h.handleScheduleRetry(ctx, sessionID, msg)

	case *IncomingTransferNotification:
		// Informational notification for UI/logging. No FSM
		// follow-up events are required.
		logger(ctx).InfoS(
			ctx,
			"Incoming transfer notification received",
			slog.String("session_id", msg.SessionID.String()),
			slog.Int("num_recipients", len(msg.Recipients)),
		)

		return nil, nil

	default:
		return nil, fmt.Errorf("unhandled outbox event %T", outbox)
	}
}

// handleArkSignatures signs the Ark PSBT inputs using the client key on the
// owner leaf path of each checkpoint output.
func (h *SigningOutboxHandler) handleArkSignatures(msg *RequestArkSignatures) (
	[]Event, error) {

	if h.Signer == nil {
		return nil, fmt.Errorf("signer is required")
	}

	err := SignArkPSBT(
		h.Signer, msg.ArkPSBT, msg.CheckpointPSBTs, msg.TransferInputs,
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
func (h *SigningOutboxHandler) handleCheckpointSignatures(ctx context.Context,
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

	logCheckpointSummary(
		ctx, "Checkpoint signatures attached",
		msg.CoSignedCheckpointPSBTs,
	)

	return []Event{
		&CheckpointsSignedEvent{
			FinalCheckpointPSBTs: msg.CoSignedCheckpointPSBTs,
		},
	}, nil
}

// logCheckpointSummary emits a compact summary of the first input metadata for
// each checkpoint PSBT. This is used to trace finalize payload fidelity across
// client/server boundaries.
func logCheckpointSummary(ctx context.Context, prefix string,
	checkpoints []*psbt.Packet) {

	for i := range checkpoints {
		checkpoint := checkpoints[i]
		if checkpoint == nil || len(checkpoint.Inputs) == 0 {
			logger(ctx).DebugS(ctx, prefix,
				slog.Int("checkpoint_index", i),
				slog.Bool("nil_checkpoint", checkpoint == nil),
			)

			continue
		}

		in := checkpoint.Inputs[0]
		logger(ctx).DebugS(ctx, prefix,
			slog.Int("checkpoint_index", i),
			slog.Int(
				"final_witness_len", len(in.FinalScriptWitness),
			),
			slog.Int(
				"taproot_sig_count",
				len(in.TaprootScriptSpendSig),
			),
			slog.Int(
				"taproot_leaf_count", len(in.TaprootLeafScript),
			),
			slog.Int("unknown_count", len(in.Unknowns)),
		)
	}
}

// handleScheduleRetry schedules a retry timer via the timeout actor. The
// session ID is encoded as the timeout ID so the expiry callback can
// reconstruct a ResumeSessionRequest targeting the correct session. When no
// timeout actor is configured, scheduling becomes a no-op and callers must
// resume explicitly.
func (h *SigningOutboxHandler) handleScheduleRetry(ctx context.Context,
	sessionID SessionID, msg *ScheduleRetryRequest) ([]Event, error) {

	if h.TimeoutActor == nil {
		return nil, nil
	}

	if h.CallbackRef == nil {
		return nil, fmt.Errorf("timeout callback ref not wired")
	}

	// Use the session ID hex string as the timeout ID. When the timer
	// fires, the callback ref's map function parses this back into a
	// SessionID for the ResumeSessionRequest.
	timeoutID := timeout.ID(sessionID.String())

	err := h.TimeoutActor.Tell(ctx, &timeout.ScheduleTimeoutRequest{
		ID:       timeoutID,
		Duration: msg.After,
		Callback: h.CallbackRef,
	})
	if err != nil {
		return nil, fmt.Errorf("schedule retry timeout: %w", err)
	}

	// The timeout actor will deliver ResumeSessionRequest asynchronously
	// via the callback ref when the timer fires.
	return nil, nil
}

var _ OutboxHandler = (*SigningOutboxHandler)(nil)
