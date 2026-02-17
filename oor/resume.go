package oor

import (
	"fmt"
)

// OutboxForState returns the outbox request implied by the current outgoing
// session state.
//
// This is used to support explicit retry/resume logic: after a restart, the app
// can either rely on durable-actor restart handling or explicitly call
// ResumeSessionRequest to re-send the submit/finalize request (or re-request
// checkpoint signing).
func OutboxForState(state State) ([]OutboxEvent, error) {
	if state == nil {
		return nil, fmt.Errorf("state must be provided")
	}

	switch s := state.(type) {
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
