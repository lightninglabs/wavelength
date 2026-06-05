package oor

import (
	"fmt"
)

// resumeOutboxForIncomingState returns the outbox to re-drive for an incoming
// state on resume. An incoming session mid-backoff on metadata resolution must
// resume by re-scheduling the retry rather than firing the query immediately:
// otherwise a restart during one of the capped backoff windows resets the wait
// to zero, and repeated restarts burn through maxMetadataRetries far faster
// than the intended schedule while re-spinning the operator mailbox. The
// persisted attempt count reproduces the same deterministic delay.
func resumeOutboxForIncomingState(state SessionState) ([]OutboxEvent, error) {
	notified, ok := state.(*ReceiveNotified)
	if ok && notified.MetadataAttempts > 0 {
		return []OutboxEvent{
			&ScheduleRetryRequest{
				After: metadataRetryBackoff(
					notified.MetadataAttempts,
				),
				Reason: "incoming metadata retry resumed " +
					"after restart",
			},
		}, nil
	}

	return OutboxForIncomingState(state)
}

// OutboxForIncomingState returns the outbox implied by the current incoming
// receive state.
func OutboxForIncomingState(state SessionState) ([]OutboxEvent, error) {
	if state == nil {
		return nil, fmt.Errorf("state must be provided")
	}

	switch s := state.(type) {
	case *ReceiveResolving:
		return []OutboxEvent{
			&QueryIncomingTransferRequest{
				SessionID: s.SessionID,
				RecipientPkScript: append(
					[]byte(nil), s.RecipientPkScript...,
				),
				RecipientEventID: s.RecipientEventID,
			},
		}, nil

	case *ReceiveNotified:
		recipients, err := ExtractArkRecipients(s.ArkPSBT)
		if err != nil {
			return nil, err
		}

		return []OutboxEvent{
			&QueryIncomingMetadataRequest{
				SessionID:            s.SessionID,
				ArkPSBT:              s.ArkPSBT,
				FinalCheckpointPSBTs: s.FinalCheckpointPSBTs,
				Recipients:           recipients,
			},
		}, nil

	case *ReceiveAwaitingAck:
		return []OutboxEvent{
			&SendIncomingAckRequest{
				SessionID: s.SessionID,
			},
		}, nil

	case *ReceiveCompleted, *Failed:
		return nil, nil

	default:
		return nil, fmt.Errorf("unsupported incoming state type: %T",
			state)
	}
}

// OutboxForState returns the outbox request implied by the current outgoing
// session state.
//
// This is used to support explicit retry/resume logic: after a restart, the app
// can either rely on durable-actor restart handling or explicitly call
// submit/finalize request (or re-request signing steps).
func OutboxForState(state State) ([]OutboxEvent, error) {
	if state == nil {
		return nil, fmt.Errorf("state must be provided")
	}

	switch s := state.(type) {
	case *AwaitingArkSignatures:
		return []OutboxEvent{
			&RequestArkSignatures{
				ArkPSBT:         s.ArkPSBT,
				CheckpointPSBTs: s.CheckpointPSBTs,
				TransferInputs:  s.TransferInputs,
			},
		}, nil

	case *AwaitingSubmitAccepted:
		return []OutboxEvent{
			&SendSubmitPackageRequest{
				ArkPSBT:         s.ArkPSBT,
				CheckpointPSBTs: s.CheckpointPSBTs,
				TransferInputs:  s.TransferInputs,
				Recipients:      s.RecipientOutputs,
			},
		}, nil

	case *AwaitingCheckpointSignatures:
		return []OutboxEvent{
			&RequestCheckpointSignatures{
				ArkPSBT: s.ArkPSBT,
				CoSignedCheckpointPSBTs: s.
					CoSignedCheckpointPSBTs,
				TransferInputs: s.TransferInputs,
			},
		}, nil

	case *AwaitingFinalizeAccepted:
		return []OutboxEvent{
			&SendFinalizePackageRequest{
				ArkPSBT:              s.ArkPSBT,
				FinalCheckpointPSBTs: s.FinalCheckpointPSBTs,
			},
		}, nil

	case *AwaitingLocalVTXOUpdate:
		return []OutboxEvent{
			&MarkInputsSpentRequest{
				Outpoints: InputOutpoints(s.TransferInputs),
			},
		}, nil

	case *Completed, *Failed:
		return nil, nil

	default:
		return nil, fmt.Errorf("unsupported outgoing state type: %T",
			state)
	}
}
