package oor

import (
	"fmt"
)

// OutboxForIncomingState returns the outbox implied by the current incoming
// receive state.
func OutboxForIncomingState(state SessionState) ([]OutboxEvent, error) {
	if state == nil {
		return nil, fmt.Errorf("state must be provided")
	}

	switch s := state.(type) {
	case *ReceiveResolving:
		return nil, nil

	case *ReceiveNotified:
		recipients, err := ExtractArkRecipients(s.ArkPSBT)
		if err != nil {
			return nil, err
		}

		return []OutboxEvent{
			&MaterializeIncomingVTXOsRequest{
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
				Outpoints: s.InputOutpoints,
			},
		}, nil

	case *RetryBackoff:
		return []OutboxEvent{
			&ScheduleRetryRequest{
				After:  s.RetryAfter,
				Reason: s.Reason,
			},
		}, nil
	case *Completed, *Failed:
		return nil, nil

	default:
		return nil, fmt.Errorf("unsupported outgoing state type: %T",
			state)
	}
}
