package oor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lightninglabs/wavelength/lib/tx/arktx"
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

// notifiedGiveUp advances the persisted attempt counter and fails the notified
// session terminally once the give-up bound is reached. It returns the
// incremented state and whether the session gave up; callers re-query (and
// re-arm the give-up timer) only while still under the bound. The bound is
// checked against the persisted counter before incrementing so a corrupted
// snapshot at math.MaxUint32 cannot wrap past the limit and bypass the give-up.
func notifiedGiveUp(state *ReceiveNotified,
	reason string) (*ReceiveNotified, *StateTransition) {

	if state.MetadataAttempts >= maxMetadataRetries {
		return nil, &StateTransition{
			NextState: &Failed{
				Reason: fmt.Sprintf("incoming metadata "+
					"unresolved after %d retries: %s",
					maxMetadataRetries, reason),
			},
			NewEvents: fn.None[EmittedEvent](),
		}
	}

	next := *state
	next.MetadataAttempts = state.MetadataAttempts + 1

	return &next, nil
}

// handleNotifiedRetry derives the backoff-or-give-up transition for a retryable
// outbox failure in the notified state (a failed metadata query or a retryable
// materialization failure). It increments the persisted attempt counter and
// arms a single backoff timer; the timer's RetryDueEvent re-drives the metadata
// query via handleNotifiedTimerRetry. Failing terminally at the bound removes a
// stuck session from the retry population rather than re-querying indefinitely.
func handleNotifiedRetry(state *ReceiveNotified,
	evt *OutboxErrorEvent) *StateTransition {

	next, giveUp := notifiedGiveUp(state, evt.ErrorReason)
	if giveUp != nil {
		return giveUp
	}

	return &StateTransition{
		NextState: next,
		NewEvents: fn.Some(EmittedEvent{
			Outbox: []OutboxEvent{
				&ScheduleRetryRequest{
					After: metadataRetryBackoff(
						next.MetadataAttempts,
					),
					Reason: evt.ErrorReason,
				},
			},
		}),
	}
}

// handleNotifiedTimerRetry derives the give-up-or-re-query transition when the
// metadata give-up timer fires. It mirrors handleResolveRetry: it advances the
// persisted attempt counter, re-queries with backoff, and fails terminally at
// the bound so an unanswered metadata lookup frees the session's concurrency
// slot rather than re-querying forever. The attempt counter rides on the
// returned state so it is persisted across restarts.
func handleNotifiedTimerRetry(state *ReceiveNotified) *StateTransition {
	next, giveUp := notifiedGiveUp(state, notifiedGiveUpReason)
	if giveUp != nil {
		return giveUp
	}

	// Re-extract recipients to rebuild the query. The Ark PSBT was already
	// validated on entry to ReceiveNotified, so this cannot fail here; on
	// the off chance it does, still re-arm the give-up timer so the session
	// keeps moving toward the terminal give-up.
	recipients, err := ExtractArkRecipients(state.ArkPSBT)
	if err != nil {
		return &StateTransition{
			NextState: next,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&ScheduleRetryRequest{
						After: metadataRetryBackoff(
							next.MetadataAttempts,
						),
						Reason: notifiedGiveUpReason,
					},
				},
			}),
		}
	}

	return &StateTransition{
		NextState: next,
		NewEvents: fn.Some(EmittedEvent{
			Outbox: notifiedOutbox(next, recipients),
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

// waitsOnGiveUpTimer reports whether the receive state arms a give-up timer
// while it waits for an operator response that has no failure path. Only these
// states advance their attempt counter on a timer expiry; both also fail
// terminally at their bound so an unanswered operator frees the session's
// concurrency slot.
func waitsOnGiveUpTimer(state SessionState) bool {
	switch state.(type) {
	case *ReceiveResolving:
		return true

	case *ReceiveNotified:
		return true

	default:
		return false
	}
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
	recipients := CloneArkRecipients(evt.Recipients)
	if len(recipients) == 0 {
		recipients, err = ExtractArkRecipients(evt.ArkPSBT)
		if err != nil {
			return nil, err
		}
	}

	notified := &ReceiveNotified{
		SessionID:            evt.SessionID,
		ArkPSBT:              evt.ArkPSBT,
		FinalCheckpointPSBTs: evt.FinalCheckpointPSBTs,
		AncestorPackages:     evt.AncestorPackages,
		Recipients:           recipients,
	}

	// The metadata query (like the phase-1 hint query) has no failure
	// response on operator silence, so arm a give-up timer alongside it.
	// Without it an operator that answers phase-1 but goes silent on the
	// metadata lookup would pin this child in ReceiveNotified forever,
	// holding an r.incoming concurrency slot.
	outbox := append(
		[]OutboxEvent{
			&IncomingTransferNotification{
				SessionID:  evt.SessionID,
				ArkPSBT:    evt.ArkPSBT,
				Recipients: recipients,
			},
		},
		notifiedOutbox(notified, recipients)...,
	)

	return &StateTransition{
		NextState: notified,
		NewEvents: fn.Some(EmittedEvent{
			Outbox: outbox,
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

	case *RetryDueEvent:
		return handleResolveRetry(s), nil

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

// handleResolveRetry derives the backoff-or-give-up transition when the
// phase-1 hint resolution give-up timer fires. The attempt counter rides on
// the returned state so it is persisted across restarts. At the bound the
// session fails terminally so it becomes reap-eligible and frees its
// concurrency slot, rather than re-querying an unanswered resolve forever.
func handleResolveRetry(state *ReceiveResolving) *StateTransition {
	// Give up once the bound is reached. The bound is checked against the
	// persisted counter before incrementing so a corrupted snapshot at
	// math.MaxUint32 cannot wrap past the limit and bypass the give-up.
	if state.ResolveAttempts >= maxResolveRetries {
		return &StateTransition{
			NextState: &Failed{
				Reason: fmt.Sprintf("incoming transfer hint "+
					"unresolved after %d retries",
					maxResolveRetries),
			},
			NewEvents: fn.None[EmittedEvent](),
		}
	}

	attempts := state.ResolveAttempts + 1

	next := *state
	next.RecipientPkScript = append(
		[]byte(nil), state.RecipientPkScript...,
	)
	next.ResolveAttempts = attempts

	return &StateTransition{
		NextState: &next,
		NewEvents: fn.Some(EmittedEvent{
			Outbox: resolvingOutbox(&next),
		}),
	}
}

// resolveGiveUpReason labels the resolve give-up timer's ScheduleRetryRequest.
const resolveGiveUpReason = "incoming resolve give-up timer"

// notifiedGiveUpReason labels the metadata give-up timer's
// ScheduleRetryRequest and the terminal failure when the timer fires after the
// metadata query has gone unanswered.
const notifiedGiveUpReason = "incoming metadata give-up timer"

// resolvingOutbox builds the phase-1 hint query and the give-up timer armed
// alongside it for a resolving session. The give-up backoff is keyed off the
// state's persisted ResolveAttempts so it grows across restarts, and the same
// pair is emitted on admission, resume, and each retry.
func resolvingOutbox(state *ReceiveResolving) []OutboxEvent {
	return []OutboxEvent{
		&QueryIncomingTransferRequest{
			SessionID: state.SessionID,
			RecipientPkScript: append(
				[]byte(nil), state.RecipientPkScript...,
			),
			RecipientEventID: state.RecipientEventID,
		},
		&ScheduleRetryRequest{
			After:  metadataRetryBackoff(state.ResolveAttempts + 1),
			Reason: resolveGiveUpReason,
		},
	}
}

// notifiedOutbox builds the phase-2 authoritative metadata query and the
// give-up timer armed alongside it for a notified session. The give-up backoff
// is keyed off the state's persisted MetadataAttempts so it grows across
// restarts, and the same pair is emitted on transition, resume, and each
// retry. Recipients are passed in because callers extract them from the Ark
// PSBT (which can fail) before building the outbox.
func notifiedOutbox(state *ReceiveNotified,
	recipients []ArkRecipientOutput) []OutboxEvent {

	return []OutboxEvent{
		&QueryIncomingMetadataRequest{
			SessionID:            state.SessionID,
			ArkPSBT:              state.ArkPSBT,
			FinalCheckpointPSBTs: state.FinalCheckpointPSBTs,
			Recipients:           recipients,
		},
		&ScheduleRetryRequest{
			After: metadataRetryBackoff(
				state.MetadataAttempts + 1,
			),
			Reason: notifiedGiveUpReason,
		},
	}
}

// ProcessEvent handles events for ReceiveNotified.
func (s *ReceiveNotified) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = env

	switch evt := event.(type) {
	case *IncomingMetadataResolvedEvent:
		recipients := CloneArkRecipients(s.Recipients)
		if len(recipients) == 0 {
			extracted, err := ExtractArkRecipients(s.ArkPSBT)
			if err != nil {
				return nil, err
			}

			recipients = extracted
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
		// The metadata give-up timer fired. Advance the persisted
		// attempt counter, re-query with backoff, and fail terminally
		// at maxMetadataRetries so sustained operator silence frees the
		// session's concurrency slot rather than re-querying forever.
		return handleNotifiedTimerRetry(s), nil

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
