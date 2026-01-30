package unroller

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// UnrollerMsg is a sealed interface for unroller messages.
type UnrollerMsg interface {
	actor.Message
	unrollerMsgSealed()
}

// UnrollRequest requests unrolling of a VTXO tree to make specific VTXOs
// spendable on-chain. After unrolling completes and CSV delays are satisfied,
// the VTXO outputs will be ready for spending by a separate sweeper actor.
type UnrollRequest struct {
	actor.BaseMessage

	// TargetVTXOs are the VTXOs we want to unroll to make spendable.
	TargetVTXOs []wire.OutPoint
}

// MessageType returns the message type for logging.
func (m *UnrollRequest) MessageType() string {
	return "UnrollRequest"
}

// unrollerMsgSealed marks this as implementing the sealed
// UnrollerMsg interface.
func (m *UnrollRequest) unrollerMsgSealed() {}

// ConfirmationEvent notifies the unroller when a transaction confirms.
type ConfirmationEvent struct {
	actor.BaseMessage

	Txid        chainhash.Hash
	BlockHeight int32
	BlockHash   chainhash.Hash
}

// MessageType returns the message type for logging.
func (m *ConfirmationEvent) MessageType() string {
	return "ConfirmationEvent"
}

// unrollerMsgSealed marks this as implementing the sealed
// UnrollerMsg interface.
func (m *ConfirmationEvent) unrollerMsgSealed() {}

// BlockEpochEvent notifies about new blocks for CSV tracking.
type BlockEpochEvent struct {
	actor.BaseMessage

	Height int32
	Hash   chainhash.Hash
}

// MessageType returns the message type for logging.
func (m *BlockEpochEvent) MessageType() string {
	return "BlockEpochEvent"
}

// unrollerMsgSealed marks this as implementing the sealed
// UnrollerMsg interface.
func (m *BlockEpochEvent) unrollerMsgSealed() {}

// GetUnrollStatusRequest queries the status of an unroll.
type GetUnrollStatusRequest struct {
	actor.BaseMessage

	VTXOOutpoint wire.OutPoint
}

// MessageType returns the message type for logging.
func (m *GetUnrollStatusRequest) MessageType() string {
	return "GetUnrollStatusRequest"
}

// unrollerMsgSealed marks this as implementing the sealed
// UnrollerMsg interface.
func (m *GetUnrollStatusRequest) unrollerMsgSealed() {}

// UnrollerResp is a sealed interface for responses.
type UnrollerResp interface {
	actor.Message
	unrollerRespSealed()
}

// UnrollStartedResp acknowledges unroll initiation.
type UnrollStartedResp struct {
	actor.BaseMessage
}

// MessageType returns the message type for logging.
func (m *UnrollStartedResp) MessageType() string {
	return "UnrollStartedResp"
}

// unrollerRespSealed marks this as implementing the sealed
// UnrollerResp interface.
func (m *UnrollStartedResp) unrollerRespSealed() {}

// UnrollStatusResp returns current unroll status.
type UnrollStatusResp struct {
	actor.BaseMessage

	Status          UnrollStatus
	CurrentLevel    int
	TotalLevels     int
	BlocksRemaining int32
}

// MessageType returns the message type for logging.
func (m *UnrollStatusResp) MessageType() string {
	return "UnrollStatusResp"
}

// unrollerRespSealed marks this as implementing the sealed
// UnrollerResp interface.
func (m *UnrollStatusResp) unrollerRespSealed() {}
