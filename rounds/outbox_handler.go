package rounds

import (
	"context"

	"github.com/btcsuite/btcd/chaincfg"
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

// Handle executes the outbox request and returns follow-up events. All
// outbox event types have been inlined into FSM transitions, so this
// always returns nil.
func (h *InProcessOutboxHandler) Handle(_ context.Context, _ RoundID,
	_ OutboxEvent) ([]Event, error) {

	return nil, nil
}

// Compile-time check that InProcessOutboxHandler implements OutboxHandler.
var _ OutboxHandler = (*InProcessOutboxHandler)(nil)
