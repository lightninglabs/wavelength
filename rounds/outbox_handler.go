package rounds

import (
	"context"
	"fmt"
)

// OutboxHandler executes FSM outbox requests and returns zero or more
// follow-up inbox events to feed back into the FSM. This mirrors the OOR
// package's OutboxHandler pattern: the FSM emits pure outbox structs
// describing side effects, and the handler performs the actual I/O, returning
// result events that drive the FSM forward.
//
// A nil handler is safe — askAndDrive simply skips the handler call and
// returns the accumulated outbox for legacy processOutbox routing.
type OutboxHandler interface {
	// Handle executes the outbox request and returns follow-up events.
	Handle(ctx context.Context, roundID RoundID,
		outbox OutboxEvent) ([]Event, error)
}

// InProcessOutboxHandler is a concrete OutboxHandler that executes outbox
// requests in-process using the provided store interfaces. It is used by
// both the actor (production wiring) and tests.
//
// Outbox event types that are not yet migrated to the handler return nil
// (no follow-up events, no error), allowing the legacy processOutbox path
// to handle them.
type InProcessOutboxHandler struct {
	roundStore RoundStore
	vtxoStore  VTXOStore
}

// NewInProcessOutboxHandler creates an InProcessOutboxHandler with the given
// store dependencies.
func NewInProcessOutboxHandler(roundStore RoundStore,
	vtxoStore VTXOStore) *InProcessOutboxHandler {

	return &InProcessOutboxHandler{
		roundStore: roundStore,
		vtxoStore:  vtxoStore,
	}
}

// Handle executes the outbox request and returns follow-up events. Outbox
// event types that are not yet migrated return nil, allowing the legacy
// processOutbox path to handle them.
func (h *InProcessOutboxHandler) Handle(ctx context.Context, _ RoundID,
	outbox OutboxEvent) ([]Event, error) {

	switch msg := outbox.(type) {
	case *PersistServerSigningReq:
		return h.handlePersistServerSigning(ctx, msg)

	case *ConfirmRoundReq:
		return h.handleConfirmRound(ctx, msg)

	default:
		return nil, nil
	}
}

// handlePersistServerSigning persists the round and its VTXOs after server
// signing completes. Returns a PersistServerSigningSucceededEvent on success
// or a PersistServerSigningFailedEvent on any persistence error.
func (h *InProcessOutboxHandler) handlePersistServerSigning(
	ctx context.Context,
	msg *PersistServerSigningReq) ([]Event, error) {

	// Build the Round struct from the request fields.
	round := &Round{
		RoundID:              msg.RoundID,
		FinalTx:              msg.FinalTx,
		VTXOTrees:            msg.VTXOTrees,
		ConnectorDescriptors: msg.ConnectorDescriptors,
		ForfeitInfos:         msg.ForfeitInfos,
		ClientRegistrations:  msg.ClientRegistrations,
		SweepKey:             msg.SweepKey,
		CSVDelay:             msg.CSVDelay,
	}

	err := h.roundStore.PersistRound(ctx, round)
	if err != nil {
		return []Event{&PersistServerSigningFailedEvent{
			Reason: fmt.Sprintf(
				"persist round: %v", err,
			),
		}}, nil
	}

	// Persist VTXOs in unconfirmed state before broadcast.
	if len(msg.VTXOTrees) > 0 {
		vtxos, err := collectVTXOs(
			msg.RoundID, msg.VTXOTrees,
			msg.ClientRegistrations,
		)
		if err != nil {
			return []Event{&PersistServerSigningFailedEvent{
				Reason: fmt.Sprintf(
					"collect VTXOs: %v", err,
				),
			}}, nil
		}

		err = h.vtxoStore.PersistVTXOs(ctx, vtxos)
		if err != nil {
			return []Event{&PersistServerSigningFailedEvent{
				Reason: fmt.Sprintf(
					"persist VTXOs: %v", err,
				),
			}}, nil
		}
	}

	return []Event{&PersistServerSigningSucceededEvent{}}, nil
}

// handleConfirmRound persists round confirmation data: marks VTXOs live,
// records forfeits, and marks the round as confirmed. Returns a
// ConfirmRoundSucceededEvent on success or a ConfirmRoundFailedEvent on
// any persistence error.
func (h *InProcessOutboxHandler) handleConfirmRound(ctx context.Context,
	msg *ConfirmRoundReq) ([]Event, error) {

	// Mark VTXOs live upon confirmation.
	if len(msg.VTXOTrees) > 0 {
		err := h.vtxoStore.MarkVTXOsLive(ctx, msg.RoundID)
		if err != nil {
			return []Event{&ConfirmRoundFailedEvent{
				Reason: fmt.Sprintf(
					"mark VTXOs live: %v", err,
				),
			}}, nil
		}
	}

	// Mark forfeited VTXOs after confirmation.
	for outpoint, info := range msg.ForfeitInfos {
		err := h.vtxoStore.MarkVTXOForfeit(
			ctx, outpoint, info,
		)
		if err != nil {
			return []Event{&ConfirmRoundFailedEvent{
				Reason: fmt.Sprintf(
					"mark VTXO forfeit: %v", err,
				),
			}}, nil
		}
	}

	// Persist the round as confirmed.
	err := h.roundStore.MarkRoundConfirmed(
		ctx, msg.RoundID, msg.BlockHeight, msg.BlockHash,
	)
	if err != nil {
		return []Event{&ConfirmRoundFailedEvent{
			Reason: fmt.Sprintf(
				"mark round confirmed: %v", err,
			),
		}}, nil
	}

	return []Event{&ConfirmRoundSucceededEvent{}}, nil
}

// Compile-time check that InProcessOutboxHandler implements OutboxHandler.
var _ OutboxHandler = (*InProcessOutboxHandler)(nil)
