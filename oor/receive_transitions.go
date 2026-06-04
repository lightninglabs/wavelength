package oor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// Incoming transfer receive flow (human-readable):
//
// ResolveIncomingTransferRequest (delivered by transport as a lightweight
// hint)
//   |
//   v
// ReceiveResolving
//   - async indexer fetch outside the actor tx
//   - callback re-enters with IncomingTransferEvent
//   |
//   v
// IncomingTransferEvent
//   |
//   v
// ReceiveIdle / ReceiveResolving
//   - structural checks (SessionID/txid consistency)
//   - canonical Ark PSBT validation (stable recipient extraction)
//   - extract recipients (for UI summary and wallet materialization)
//   emits outbox (in order):
//   1) IncomingTransferNotification: app/UI summary of the transfer
//   2) QueryIncomingMetadataRequest: authoritative indexer metadata lookup
//   |
//   v
// ReceiveNotified
//   - waits for IncomingMetadataResolvedEvent then emits local materialization
//   - waits for IncomingHandledEvent after local materialization completes
//   |
//   v
// ReceiveAwaitingAck
//   - emits SendIncomingAckRequest after durable materialization succeeds
//   |
//   v
// ReceiveCompleted

// unexpectedReceiveEvent returns a transition that keeps the current state and
// emits no outbox work for an unexpected event.
func unexpectedReceiveEvent(state ReceiveState, event Event) *StateTransition {
	_ = event

	return &StateTransition{
		NextState: state,
		NewEvents: fn.None[EmittedEvent](),
	}
}

// handleReceiveOutboxError derives the receive-FSM retry/fail transition for
// one outbox execution error.
func handleReceiveOutboxError(state ReceiveState,
	evt *OutboxErrorEvent) (*StateTransition, error) {

	if evt == nil {
		return nil, fmt.Errorf("outbox error event must be provided")
	}

	if !evt.Retryable {
		return &StateTransition{
			NextState: &Failed{
				Reason: evt.ErrorReason,
			},
			NewEvents: fn.None[EmittedEvent](),
		}, nil
	}

	// The notified state retries the authoritative metadata resolution.
	// Apply bounded exponential backoff and a terminal give-up so a
	// session whose VTXO never lands in the indexer stops re-querying the
	// mailbox forever instead of spinning at a flat cadence.
	if notified, ok := state.(*ReceiveNotified); ok {
		return handleNotifiedRetry(notified, evt), nil
	}

	after := evt.RetryAfter
	if after == 0 {
		after = defaultRetryDelay
	}

	return &StateTransition{
		NextState: state,
		NewEvents: fn.Some(EmittedEvent{
			Outbox: []OutboxEvent{
				&ScheduleRetryRequest{
					After:  after,
					Reason: evt.ErrorReason,
				},
			},
		}),
	}, nil
}

// handleNotifiedRetry derives the backoff-or-give-up transition for a
// retryable metadata resolution failure in the notified state. The attempt
// counter rides on the returned state so it is persisted across restarts.
func handleNotifiedRetry(state *ReceiveNotified,
	evt *OutboxErrorEvent) *StateTransition {

	// Give up once the bound is reached. The session can no longer make
	// progress, so failing it terminally removes it from the retry
	// population rather than re-querying indefinitely. The bound is
	// checked against the persisted counter before incrementing so a
	// corrupted snapshot at math.MaxUint32 cannot wrap past the limit and
	// bypass the give-up.
	if state.MetadataAttempts >= maxMetadataRetries {
		return &StateTransition{
			NextState: &Failed{
				Reason: fmt.Sprintf("incoming metadata "+
					"unresolved after %d retries: %s",
					maxMetadataRetries, evt.ErrorReason),
			},
			NewEvents: fn.None[EmittedEvent](),
		}
	}

	attempts := state.MetadataAttempts + 1

	next := *state
	next.MetadataAttempts = attempts

	return &StateTransition{
		NextState: &next,
		NewEvents: fn.Some(EmittedEvent{
			Outbox: []OutboxEvent{
				&ScheduleRetryRequest{
					After:  metadataRetryBackoff(attempts),
					Reason: evt.ErrorReason,
				},
			},
		}),
	}
}

// metadataRetryBackoff returns the delay before the next metadata retry. It
// grows exponentially with the attempt count, capped at metadataRetryMaxDelay.
// It is deterministic (no jitter) so FSM replay and snapshot round-trips stay
// reproducible.
func metadataRetryBackoff(attempt uint32) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	// Cap the shift well below the int64 overflow point; the delay is
	// clamped to the max below long before this matters.
	shift := attempt - 1
	if shift > 30 {
		shift = 30
	}

	delay := metadataRetryBaseDelay << shift
	if delay <= 0 || delay > metadataRetryMaxDelay {
		return metadataRetryMaxDelay
	}

	return delay
}

// retryReceiveState re-emits the outbox implied by the current receive state
// when a retry timer fires.
func retryReceiveState(state ReceiveState) (*StateTransition, error) {
	outbox, err := OutboxForIncomingState(state)
	if err != nil {
		return nil, err
	}

	return &StateTransition{
		NextState: state,
		NewEvents: fn.Some(EmittedEvent{
			Outbox: outbox,
		}),
	}, nil
}

// transitionIncomingTransfer validates and applies a fully resolved incoming
// transfer event, moving the receive FSM into the notified/materialize phase.
func transitionIncomingTransfer(evt *IncomingTransferEvent) (*StateTransition,
	error) {

	if evt.ArkPSBT == nil || evt.ArkPSBT.UnsignedTx == nil {
		return nil, fmt.Errorf("ark psbt must be provided")
	}

	if evt.SessionID == (SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	txid := evt.ArkPSBT.UnsignedTx.TxHash()
	if SessionID(txid) != evt.SessionID {
		return nil, fmt.Errorf("ark txid mismatch")
	}

	// Canonical ordering checks prevent subtle malleability in how the
	// recipients are extracted and displayed.
	err := arktx.ValidateCanonicalPSBT(evt.ArkPSBT)
	if err != nil {
		return nil, err
	}

	// The FSM intentionally does not decide which outputs belong to the
	// local wallet. Ownership lives behind the materialization boundary.
	recipients, err := ExtractArkRecipients(evt.ArkPSBT)
	if err != nil {
		return nil, err
	}

	return &StateTransition{
		NextState: &ReceiveNotified{
			SessionID:            evt.SessionID,
			ArkPSBT:              evt.ArkPSBT,
			FinalCheckpointPSBTs: evt.FinalCheckpointPSBTs,
			AncestorPackages:     evt.AncestorPackages,
		},
		NewEvents: fn.Some(EmittedEvent{
			Outbox: []OutboxEvent{
				&IncomingTransferNotification{
					SessionID:  evt.SessionID,
					ArkPSBT:    evt.ArkPSBT,
					Recipients: recipients,
				},
				&QueryIncomingMetadataRequest{
					SessionID: evt.SessionID,
					ArkPSBT:   evt.ArkPSBT,
					FinalCheckpointPSBTs: evt.
						FinalCheckpointPSBTs, //nolint:ll
					Recipients: recipients,
				},
			},
		}),
	}, nil
}

// ProcessEvent handles events for ReceiveIdle.
func (s *ReceiveIdle) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = env

	switch evt := event.(type) {
	case *IncomingTransferEvent:
		return transitionIncomingTransfer(evt)

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{
				Reason: evt.Reason,
			},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		logger(ctx).WarnS(ctx, "Unexpected event in receive FSM",
			nil,
			slog.String("state", fmt.Sprintf("%T", s)),
			slog.String("event_type", fmt.Sprintf("%T", event)),
		)

		return unexpectedReceiveEvent(s, event), nil
	}
}

// ProcessEvent handles events for ReceiveResolving.
func (s *ReceiveResolving) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = env

	switch evt := event.(type) {
	case *IncomingTransferEvent:
		return transitionIncomingTransfer(evt)

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{
				Reason: evt.Reason,
			},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		logger(ctx).WarnS(ctx, "Unexpected event in receive FSM",
			nil,
			slog.String("state", fmt.Sprintf("%T", s)),
			slog.String("event_type", fmt.Sprintf("%T", event)),
		)

		return unexpectedReceiveEvent(s, event), nil
	}
}

// ProcessEvent handles events for ReceiveNotified.
func (s *ReceiveNotified) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = env

	switch evt := event.(type) {
	case *IncomingMetadataResolvedEvent:
		recipients, err := ExtractArkRecipients(s.ArkPSBT)
		if err != nil {
			return nil, err
		}

		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&MaterializeIncomingVTXOsRequest{
						SessionID: s.SessionID,
						ArkPSBT:   s.ArkPSBT,
						FinalCheckpointPSBTs: s.
							FinalCheckpointPSBTs,
						Recipients:      recipients,
						MetadataMatches: evt.Matches,
						AncestorPackages: s.
							AncestorPackages,
					},
				},
			}),
		}, nil

	case *IncomingHandledEvent:
		// The application signals it has processed the
		// notification. Wallet state has been updated
		// (materialization complete), so we can now ack to the
		// server.
		_ = evt

		return &StateTransition{
			NextState: &ReceiveAwaitingAck{
				SessionID: s.SessionID,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&SendIncomingAckRequest{
						SessionID: s.SessionID,
					},
				},
			}),
		}, nil

	case *OutboxErrorEvent:
		return handleReceiveOutboxError(s, evt)

	case *RetryDueEvent:
		return retryReceiveState(s)

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{
				Reason: evt.Reason,
			},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		logger(ctx).WarnS(ctx, "Unexpected event in receive FSM",
			nil,
			slog.String("state", fmt.Sprintf("%T", s)),
			slog.String("event_type", fmt.Sprintf("%T", event)),
		)

		return unexpectedReceiveEvent(s, event), nil
	}
}

// ProcessEvent handles events for ReceiveCompleted.
func (s *ReceiveCompleted) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = env

	logger(ctx).WarnS(ctx, "Unexpected event in receive FSM",
		nil,
		slog.String("state", fmt.Sprintf("%T", s)),
		slog.String("event_type", fmt.Sprintf("%T", event)),
	)

	return unexpectedReceiveEvent(s, event), nil
}

// ProcessEvent handles events for ReceiveAwaitingAck.
func (s *ReceiveAwaitingAck) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = env

	switch evt := event.(type) {
	case *IncomingAckSentEvent:
		_ = evt

		return &StateTransition{
			NextState: &ReceiveCompleted{},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	case *OutboxErrorEvent:
		return &StateTransition{
			NextState: &Failed{
				Reason: evt.ErrorReason,
			},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{
				Reason: evt.Reason,
			},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		logger(ctx).WarnS(ctx, "Unexpected event in receive FSM",
			nil,
			slog.String("state", fmt.Sprintf("%T", s)),
			slog.String("event_type", fmt.Sprintf("%T", event)),
		)

		return unexpectedReceiveEvent(s, event), nil
	}
}
