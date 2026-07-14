package oor

import (
	"fmt"
	"time"
)

// outgoingTransportRedriveInterval is the cadence at which an outgoing wait
// state re-drives its cross-actor transport request when no operator response
// has arrived. The outgoing transport deliberately has NO terminal give-up:
// failing a transfer after the submit point-of-no-return would be unsafe, and a
// peer may simply be offline (a mobile wallet, a server down for maintenance),
// so the session waits and keeps re-driving the idempotent transport on this
// flat cadence until the operator answers. Unlike the incoming give-up timers
// it does not grow with a persisted attempt count and never fails the session;
// its sole job is to break a dead-letter wedge by re-sending rather than
// stalling until the next daemon restart.
const outgoingTransportRedriveInterval = 30 * time.Second

// submitRedriveReason labels the re-drive timer armed alongside the submit
// package request.
const submitRedriveReason = "outgoing submit re-drive timer"

// finalizeRedriveReason labels the re-drive timer armed alongside the finalize
// package request.
const finalizeRedriveReason = "outgoing finalize re-drive timer"

// localVTXOUpdateRedriveReason labels the re-drive timer armed alongside the
// input-spend completion request.
const localVTXOUpdateRedriveReason = "outgoing local-vtxo-update re-drive timer"

// resumeOutboxForIncomingState returns the outbox to re-drive for an incoming
// state on a boot restore. A session waiting on a give-up timer must resume by
// re-arming the timer from its persisted attempt count rather than firing the
// query immediately: otherwise a restart during one of the capped backoff
// windows resets the wait to zero, and repeated restarts burn through the
// give-up budget far faster than the intended schedule while re-spinning the
// operator mailbox. The persisted attempt count reproduces the same
// deterministic delay, and only the timer-driven RetryDueEvent advances the
// counter (see handleResumeSession), so boot resume never amplifies it.
func resumeOutboxForIncomingState(state SessionState) ([]OutboxEvent, error) {
	switch s := state.(type) {
	case *ReceiveResolving:
		return []OutboxEvent{
			&ScheduleRetryRequest{
				After: metadataRetryBackoff(
					s.ResolveAttempts + 1,
				),
				Reason: resolveGiveUpReason,
			},
		}, nil

	case *ReceiveNotified:
		return []OutboxEvent{
			&ScheduleRetryRequest{
				After: metadataRetryBackoff(
					s.MetadataAttempts + 1,
				),
				Reason: notifiedGiveUpReason,
			},
		}, nil

	default:
		return OutboxForIncomingState(state)
	}
}

// OutboxForIncomingState returns the outbox implied by the current incoming
// receive state.
func OutboxForIncomingState(state SessionState) ([]OutboxEvent, error) {
	if state == nil {
		return nil, fmt.Errorf("state must be provided")
	}

	switch s := state.(type) {
	case *ReceiveResolving:
		// Emit the phase-1 hint query and arm a give-up timer alongside
		// it. The phase-1 query has no failure response on operator
		// silence, so without this timer an unanswered resolve would
		// pin the session in ReceiveResolving forever. The timer expiry
		// drives a RetryDueEvent (see handleResumeSession) which either
		// re-queries with backoff or fails the session at
		// maxResolveRetries, freeing its concurrency slot.
		return resolvingOutbox(s), nil

	case *ReceiveNotified:
		// Emit the phase-2 metadata query and arm a give-up timer
		// alongside it. Like the phase-1 query, the metadata lookup has
		// no failure response on operator silence, so without this
		// timer an operator that answers phase-1 then goes silent would
		// pin the session in ReceiveNotified forever. The timer expiry
		// drives a RetryDueEvent (see handleResumeSession) which either
		// re-queries with backoff or fails the session at
		// maxMetadataRetries, freeing its concurrency slot.
		recipients := CloneArkRecipients(s.Recipients)
		if len(recipients) == 0 {
			extracted, err := ExtractArkRecipients(s.ArkPSBT)
			if err != nil {
				return nil, err
			}

			recipients = extracted
		}

		return notifiedOutbox(s, recipients), nil

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
		return submitOutbox(s), nil

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
		return finalizeOutbox(s), nil

	case *AwaitingLocalVTXOUpdate:
		return localVTXOUpdateOutbox(s), nil

	case *Completed, *Failed:
		return nil, nil

	default:
		return nil, fmt.Errorf("unsupported outgoing state type: %T",
			state)
	}
}

// submitOutbox builds the submit package request and the re-drive timer armed
// alongside it for a session waiting on submit acceptance. The submit transport
// is delivered cross-actor to serverconn and the operator's co-sign response
// arrives as a fresh event; on operator silence there is no failure path (and
// none is wanted: the peer may just be offline), so without this timer a
// transient delivery failure that exhausts the actor delivery store and
// dead-letters would pin the session in AwaitingSubmitAccepted until a daemon
// restart. The timer expiry re-drives this same outbox (the transport is
// idempotent: the server dedups duplicate submits), self-healing the wedge. The
// same pair is emitted on transition and on resume.
func submitOutbox(state *AwaitingSubmitAccepted) []OutboxEvent {
	return []OutboxEvent{
		&SendSubmitPackageRequest{
			ArkPSBT:         state.ArkPSBT,
			CheckpointPSBTs: state.CheckpointPSBTs,
			TransferInputs:  state.TransferInputs,
			Recipients:      state.RecipientOutputs,
			TaprootAssetTransfer: state.
				TaprootAssetTransfer,
		},
		&ScheduleRetryRequest{
			After:  outgoingTransportRedriveInterval,
			Reason: submitRedriveReason,
		},
	}
}

// finalizeOutbox builds the finalize package request and the re-drive timer
// armed alongside it for a session waiting on finalize acceptance. Like the
// submit transport, the finalize ack has no failure path on operator silence
// (and must not: the peer may simply be offline), so the timer breaks a
// dead-letter wedge by re-driving the idempotent finalize transport. The same
// pair is emitted on transition and on resume.
func finalizeOutbox(state *AwaitingFinalizeAccepted) []OutboxEvent {
	return []OutboxEvent{
		&SendFinalizePackageRequest{
			ArkPSBT:              state.ArkPSBT,
			FinalCheckpointPSBTs: state.FinalCheckpointPSBTs,
		},
		&ScheduleRetryRequest{
			After:  outgoingTransportRedriveInterval,
			Reason: finalizeRedriveReason,
		},
	}
}

// localVTXOUpdateOutbox builds the input-spend completion request and the
// re-drive timer armed alongside it for a session resumed in
// AwaitingLocalVTXOUpdate. Unlike the submit/finalize waits, this state is not
// driven by an operator response: the only thing that advances it is
// completeSpend succeeding. Without the re-drive timer a resume whose
// completeSpend fails (a transient VTXO-manager error) would re-emit only the
// spend request once and then wedge in AwaitingLocalVTXOUpdate until the next
// daemon restart. The timer re-drives the idempotent spend on a bounded cadence
// (isPersistedSpent absorbs the replay), self-healing the wedge.
func localVTXOUpdateOutbox(state *AwaitingLocalVTXOUpdate) []OutboxEvent {
	return []OutboxEvent{
		&MarkInputsSpentRequest{
			Outpoints: InputOutpoints(state.TransferInputs),
		},
		&ScheduleRetryRequest{
			After:  outgoingTransportRedriveInterval,
			Reason: localVTXOUpdateRedriveReason,
		},
	}
}
