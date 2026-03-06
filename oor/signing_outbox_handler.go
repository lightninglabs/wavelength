package oor

import (
	"context"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/input"
)

// RetryScheduler arranges for a RetryDueEvent to be delivered to the OOR
// actor after the requested delay. Implementations typically use
// time.AfterFunc to schedule a DriveEventRequest containing the
// RetryDueEvent back into the actor mailbox.
type RetryScheduler func(sessionID SessionID, after time.Duration,
	reason string)

// SigningOutboxHandler implements the signing and scheduling outbox requests
// emitted by the OOR FSM.
//
// This handler is intended to be used as the Next delegate inside
// LocalPersistenceOutboxHandler. It handles the following outbox events:
//
//   - RequestArkSignatures: v0 pass-through (no additional local signing
//     needed beyond deterministic package construction).
//   - RequestCheckpointSignatures: attaches client-side collaborative VTXO
//     spend signatures to each checkpoint PSBT via SignCheckpointPSBTs.
//   - ScheduleRetryRequest: delegates to the configured RetryScheduler.
//   - IncomingTransferNotification: informational no-op (logging only).
type SigningOutboxHandler struct {
	// Signer signs checkpoint inputs at RequestCheckpointSignatures.
	Signer input.Signer

	// ScheduleRetry arranges for a RetryDueEvent to be delivered after
	// the requested delay. When nil, retry requests return
	// RetryDueEvent immediately.
	ScheduleRetry RetryScheduler
}

// Handle executes one outbox request and returns follow-up FSM events.
func (h *SigningOutboxHandler) Handle(ctx context.Context,
	sessionID SessionID, outbox OutboxEvent) ([]Event, error) {

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		// v0 does not require extra local Ark signing beyond
		// deterministic package construction. Forward the Ark
		// PSBT as-is.
		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *RequestCheckpointSignatures:
		return h.handleCheckpointSignatures(msg)

	case *ScheduleRetryRequest:
		return h.handleScheduleRetry(sessionID, msg)

	case *IncomingTransferNotification:
		// Informational notification for UI/logging. No FSM
		// follow-up events are required.
		return nil, nil

	default:
		return nil, fmt.Errorf("unhandled outbox event %T", outbox)
	}
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

// handleScheduleRetry delegates retry scheduling to the configured
// RetryScheduler. When no scheduler is configured, a RetryDueEvent is
// returned immediately for backward compatibility with test harnesses.
func (h *SigningOutboxHandler) handleScheduleRetry(sessionID SessionID,
	msg *ScheduleRetryRequest) ([]Event, error) {

	if h.ScheduleRetry != nil {
		h.ScheduleRetry(sessionID, msg.After, msg.Reason)

		// The scheduler will deliver RetryDueEvent
		// asynchronously via the actor mailbox.
		return nil, nil
	}

	// No scheduler configured: emit RetryDueEvent immediately.
	return []Event{&RetryDueEvent{}}, nil
}

var _ OutboxHandler = (*SigningOutboxHandler)(nil)
