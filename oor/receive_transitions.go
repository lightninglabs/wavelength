package oor

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// unexpectedReceiveEvent returns a transition that keeps the current state and
// emits no outbox work for an unexpected event.
func unexpectedReceiveEvent(state ReceiveState, event Event) *StateTransition {
	_ = event

	return &StateTransition{
		NextState: state,
		NewEvents: fn.None[EmittedEvent](),
	}
}

// ProcessEvent handles events for ReceiveIdle.
func (s *ReceiveIdle) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *IncomingTransferEvent:
		// Basic sanity checks: we only accept an incoming transfer
		// notification if it is self-consistent (Ark txid matches
		// SessionID) and structurally valid.
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

		// Canonical ordering checks prevent subtle malleability in how
		// the recipients are extracted and displayed.
		//
		// The goal is that all parties derive identical semantics from
		// identical bytes.
		err := arktx.ValidateCanonicalPSBT(evt.ArkPSBT)
		if err != nil {
			return nil, err
		}

		// Extract recipients and surface the notification to the
		// application layer.
		recipients, err := ExtractArkRecipients(evt.ArkPSBT)
		if err != nil {
			return nil, err
		}

		// The outbox is intentionally ordered:
		//   1) notify the app/UI so it can show the transfer;
		//   2) materialize incoming VTXOs into local state; and
		//   3) ack receipt to the server (best-effort, idempotent).
		return &StateTransition{
			NextState: &ReceiveNotified{
				SessionID: evt.SessionID,
				ArkPSBT:   evt.ArkPSBT,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&IncomingTransferNotification{
						SessionID:  evt.SessionID,
						ArkPSBT:    evt.ArkPSBT,
						Recipients: recipients,
					},
					&MaterializeIncomingVTXOsRequest{
						SessionID:  evt.SessionID,
						ArkPSBT:    evt.ArkPSBT,
						Recipients: recipients,
					},
					&SendIncomingAckRequest{
						SessionID: evt.SessionID,
					},
				},
			}),
		}, nil

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedReceiveEvent(s, event), nil
	}
}

// ProcessEvent handles events for ReceiveNotified.
func (s *ReceiveNotified) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *IncomingHandledEvent:
		// The application signals it has processed the notification.
		// Wallet state has been updated (materialization complete).
		_ = evt

		return &StateTransition{
			NextState: &ReceiveCompleted{},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedReceiveEvent(s, event), nil
	}
}

// ProcessEvent handles events for ReceiveCompleted.
func (s *ReceiveCompleted) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	return unexpectedReceiveEvent(s, event), nil
}
