package oor

import (
	"context"
	"fmt"
	"log/slog"

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

// ignoreReceiveEvent returns a transition that keeps the current state and
// emits no outbox work for an event that the current receive state does not
// handle.
func ignoreReceiveEvent(state ReceiveState, event Event) *StateTransition {
	_ = event

	return &StateTransition{
		NextState: state,
		NewEvents: fn.None[EmittedEvent](),
	}
}

// transitionIncomingTransfer validates and applies a fully resolved incoming
// transfer event, moving the receive FSM into the notified/materialize phase.
func transitionIncomingTransfer(
	evt *IncomingTransferEvent) (*StateTransition, error) {

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
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		log.WarnS(ctx, "Unexpected event in receive FSM", nil,
			slog.String("state", fmt.Sprintf("%T", s)),
			slog.String("event_type", fmt.Sprintf("%T", event)))

		return ignoreReceiveEvent(s, event), nil
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
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		log.WarnS(ctx, "Unexpected event in receive FSM", nil,
			slog.String("state", fmt.Sprintf("%T", s)),
			slog.String("event_type", fmt.Sprintf("%T", event)))

		return ignoreReceiveEvent(s, event), nil
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
		return &StateTransition{
			NextState: &Failed{Reason: evt.ErrorReason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		log.WarnS(ctx, "Unexpected event in receive FSM", nil,
			slog.String("state", fmt.Sprintf("%T", s)),
			slog.String("event_type", fmt.Sprintf("%T", event)))

		return ignoreReceiveEvent(s, event), nil
	}
}

// ProcessEvent handles events for ReceiveCompleted.
func (s *ReceiveCompleted) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = env

	log.WarnS(ctx, "Unexpected event in receive FSM", nil,
		slog.String("state", fmt.Sprintf("%T", s)),
		slog.String("event_type", fmt.Sprintf("%T", event)))

	return ignoreReceiveEvent(s, event), nil
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
			NextState: &Failed{Reason: evt.ErrorReason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		log.WarnS(ctx, "Unexpected event in receive FSM", nil,
			slog.String("state", fmt.Sprintf("%T", s)),
			slog.String("event_type", fmt.Sprintf("%T", event)))

		return ignoreReceiveEvent(s, event), nil
	}
}
