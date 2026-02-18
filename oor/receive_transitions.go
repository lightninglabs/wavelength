package oor

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// Incoming transfer receive flow (human-readable):
//
// IncomingTransferEvent (delivered by transport; server believes we are a
// recipient)
//   |
//   v
// ReceiveIdle
//   - structural checks (SessionID/txid consistency)
//   - canonical Ark PSBT validation (stable recipient extraction)
//   - extract recipients (for UI summary and wallet materialization)
//   emits outbox (in order):
//   1) IncomingTransferNotification: app/UI summary of the transfer
//   2) MaterializeIncomingVTXOsRequest: wallet/state update (filter + persist)
//   3) SendIncomingAckRequest: best-effort ack to server
//   |
//   v
// ReceiveNotified
//   - waits for IncomingHandledEvent (app confirms it processed the transfer)
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
		//
		// Note: the FSM intentionally does not decide which of these
		// outputs belong to the local wallet. That check depends on
		// local wallet keys and policy, so it lives behind the outbox
		// boundary in the materialization step.
		recipients, err := ExtractArkRecipients(evt.ArkPSBT)
		if err != nil {
			return nil, err
		}

		// The outbox is intentionally ordered:
		//   1) notify the app/UI so it can show the transfer;
		//   2) materialize incoming VTXOs into local state.
		//
		// Ack is emitted only after the application confirms incoming
		// handling to avoid acknowledging transfers that were not
		// durably materialized yet.
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
						SessionID: evt.SessionID,
						ArkPSBT:   evt.ArkPSBT,
						FinalCheckpointPSBTs: evt.
							FinalCheckpointPSBTs,
						Recipients: recipients,
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
		// Wallet state has been updated (materialization complete), so
		// we can now ack to the server.
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

// ProcessEvent handles events for ReceiveAwaitingAck.
func (s *ReceiveAwaitingAck) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *IncomingAckSentEvent:
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
