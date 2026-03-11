package rounds

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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
// requests in-process using the provided store and wallet interfaces. It is
// used by both the actor (production wiring) and tests.
//
// Outbox event types that are not yet migrated to the handler return nil
// (no follow-up events, no error), allowing the legacy processOutbox path
// to handle them.
type InProcessOutboxHandler struct {
	roundStore          RoundStore
	vtxoStore           VTXOStore
	walletController    WalletController
	feeEstimator        chainfee.Estimator
	boardingInputLocker BoardingInputLocker
	vtxoLocker          vtxo.Locker
	chainSource         ChainSource
	chainParams         *chaincfg.Params
	terms               *batch.Terms
	log                 btclog.Logger
	confTarget          uint32
	minConfs            int32
	walletAccount       string
}

// NewInProcessOutboxHandler creates an InProcessOutboxHandler with the given
// store, wallet, locking, validation, and fee estimation dependencies.
func NewInProcessOutboxHandler(roundStore RoundStore,
	vtxoStore VTXOStore, walletController WalletController,
	feeEstimator chainfee.Estimator,
	boardingInputLocker BoardingInputLocker,
	vtxoLocker vtxo.Locker, chainSource ChainSource,
	chainParams *chaincfg.Params, terms *batch.Terms,
	log btclog.Logger, confTarget uint32,
	minConfs int32,
	walletAccount string) *InProcessOutboxHandler {

	return &InProcessOutboxHandler{
		roundStore:          roundStore,
		vtxoStore:           vtxoStore,
		walletController:    walletController,
		feeEstimator:        feeEstimator,
		boardingInputLocker: boardingInputLocker,
		vtxoLocker:          vtxoLocker,
		chainSource:         chainSource,
		chainParams:         chainParams,
		terms:               terms,
		log:                 log,
		confTarget:          confTarget,
		minConfs:            minConfs,
		walletAccount:       walletAccount,
	}
}

// Handle executes the outbox request and returns follow-up events. Outbox
// event types that are not yet migrated return nil, allowing the legacy
// processOutbox path to handle them.
func (h *InProcessOutboxHandler) Handle(ctx context.Context, _ RoundID,
	outbox OutboxEvent) ([]Event, error) {

	switch msg := outbox.(type) {
	case *SignAndFinalizeRoundReq:
		return h.handleSignAndFinalize(ctx, msg)

	case *PersistServerSigningReq:
		return h.handlePersistServerSigning(ctx, msg)

	case *ConfirmRoundReq:
		return h.handleConfirmRound(ctx, msg)

	default:
		return nil, nil
	}
}

// handleSignAndFinalize signs all boarding inputs, completes forfeit
// transactions, and finalizes the PSBT. Returns a
// SignAndFinalizeSucceededEvent on success or a
// SignAndFinalizeFailedEvent on any signing/finalization error.
func (h *InProcessOutboxHandler) handleSignAndFinalize(ctx context.Context,
	msg *SignAndFinalizeRoundReq) ([]Event, error) {

	// Sign all boarding inputs with the collected client signatures
	// and the operator's signatures.
	err := signBoardingInputs(
		msg.PSBT, msg.CollectedSignatures,
		msg.ClientRegistrations, h.walletController,
	)
	if err != nil {
		return []Event{&SignAndFinalizeFailedEvent{
			Reason: fmt.Sprintf(
				"sign boarding inputs: %v", err,
			),
		}}, nil
	}

	forfeitInfos := make(map[wire.OutPoint]*ForfeitInfo)

	// Complete forfeit transactions with the server's signatures.
	for clientID, reg := range msg.ClientRegistrations {
		if len(reg.ForfeitInputs) == 0 {
			continue
		}

		if len(msg.ConnectorAssignments) == 0 {
			return []Event{&SignAndFinalizeFailedEvent{
				Reason: fmt.Sprintf(
					"connector assignments missing "+
						"for client %s", clientID,
				),
			}}, nil
		}

		forfeitTxs, ok := msg.CollectedForfeitTxs[clientID]
		if !ok {
			return []Event{&SignAndFinalizeFailedEvent{
				Reason: fmt.Sprintf(
					"missing forfeit txs for "+
						"client %s", clientID,
				),
			}}, nil
		}

		spent, err := completeForfeitTxs(
			forfeitTxs, reg, msg.ConnectorAssignments,
			h.walletController, msg.OperatorKey,
			msg.VTXOExitDelay, msg.RoundID,
		)
		if err != nil {
			return []Event{&SignAndFinalizeFailedEvent{
				Reason: fmt.Sprintf(
					"complete forfeit txs for "+
						"client %s: %v",
					clientID, err,
				),
			}}, nil
		}

		for _, spentVTXO := range spent {
			if spentVTXO.ForfeitInfo == nil {
				return []Event{&SignAndFinalizeFailedEvent{
					Reason: fmt.Sprintf(
						"missing forfeit info "+
							"for client %s",
						clientID,
					),
				}}, nil
			}

			forfeitInfos[spentVTXO.VTXOOutpoint] =
				spentVTXO.ForfeitInfo
		}
	}

	// Finalize the PSBT which signs all wallet-controlled inputs.
	finalTx, err := h.walletController.FinalizePsbt(ctx, msg.PSBT)
	if err != nil {
		return []Event{&SignAndFinalizeFailedEvent{
			Reason: fmt.Sprintf(
				"finalize PSBT: %v", err,
			),
		}}, nil
	}

	return []Event{&SignAndFinalizeSucceededEvent{
		FinalTx:      finalTx,
		ForfeitInfos: forfeitInfos,
	}}, nil
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
