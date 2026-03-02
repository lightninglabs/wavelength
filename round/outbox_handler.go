package round

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/types"
)

// OutboxRequest is a sealed marker interface for outbox messages that
// represent I/O side-effect requests. These are emitted by FSM
// transitions and handled by the OutboxHandler, which performs the
// actual I/O and returns follow-up ClientEvents to feed back into the
// FSM. Non-request outbox messages (server messages, notifications)
// continue through the existing processOutbox path.
type OutboxRequest interface {
	ClientOutMsg

	// outboxRequestSealed prevents external implementations,
	// ensuring only this package can define request types.
	outboxRequestSealed()
}

// OutboxHandler processes OutboxRequest messages emitted by the FSM.
// Each request represents an I/O side effect (persistence, signing,
// key derivation) that was previously performed inline during state
// transitions. The handler performs the I/O and returns follow-up
// ClientEvent(s) that the event pump feeds back into the FSM.
type OutboxHandler interface {
	// Handle processes an OutboxRequest and returns zero or more
	// follow-up events. The caller feeds these events back into the
	// FSM via the askAndDrive event pump. An error return indicates
	// a fatal handler failure.
	Handle(ctx context.Context,
		msg OutboxRequest) ([]ClientEvent, error)
}

// InProcessOutboxHandler performs I/O side effects in the same process
// using the provided stores and wallet. This is the default handler
// wired by the actor; tests may substitute a mock.
type InProcessOutboxHandler struct {
	// RoundStore provides round checkpoint persistence.
	RoundStore RoundStore

	// VTXOStore provides VTXO persistence and lookup.
	VTXOStore VTXOStore

	// Wallet provides signing and key derivation.
	Wallet ClientWallet

	// QueryBestHeight returns the current chain tip height.
	QueryBestHeight func(context.Context) (uint32, error)

	// OperatorTerms contains the operator's parameters.
	OperatorTerms *types.OperatorTerms

	// ChainParams are the Bitcoin network parameters.
	ChainParams *chaincfg.Params

	// MaxOperatorFee is the maximum acceptable operator fee.
	MaxOperatorFee btcutil.Amount

	// DisableJoinRequestAuth skips BIP-322 join authorization in
	// tests.
	DisableJoinRequestAuth bool

	// Log is the logger for handler operations.
	Log btclog.Logger
}

// SaveVTXOsReq requests persistence of newly created VTXOs after a
// round's commitment transaction confirms. The handler calls
// VTXOStore.SaveVTXOs and returns SaveVTXOsSucceeded or
// SaveVTXOsFailed.
type SaveVTXOsReq struct {
	// VTXOs are the client VTXOs to persist.
	VTXOs []*ClientVTXO
}

func (r *SaveVTXOsReq) clientOutMsgSealed()   {}
func (r *SaveVTXOsReq) outboxRequestSealed()  {}

// Compile-time assertion that InProcessOutboxHandler implements
// OutboxHandler.
var _ OutboxHandler = (*InProcessOutboxHandler)(nil)

// Handle dispatches an OutboxRequest to the appropriate handler
// method. Unrecognized request types return an error.
func (h *InProcessOutboxHandler) Handle(ctx context.Context,
	msg OutboxRequest) ([]ClientEvent, error) {

	switch req := msg.(type) {
	case *SaveVTXOsReq:
		return h.handleSaveVTXOs(ctx, req)

	default:
		return nil, fmt.Errorf("unhandled outbox request "+
			"type: %T", msg)
	}
}

// handleSaveVTXOs persists VTXOs via VTXOStore and returns the
// appropriate follow-up event.
func (h *InProcessOutboxHandler) handleSaveVTXOs(ctx context.Context,
	req *SaveVTXOsReq) ([]ClientEvent, error) {

	if err := h.VTXOStore.SaveVTXOs(ctx, req.VTXOs); err != nil {
		return []ClientEvent{
			&SaveVTXOsFailed{Error: err},
		}, nil
	}

	return []ClientEvent{&SaveVTXOsSucceeded{}}, nil
}
