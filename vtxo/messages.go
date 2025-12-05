package vtxo

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/round"
)

// ManagerMsg is the message type accepted by the VTXO Manager actor.
type ManagerMsg interface {
	actor.Message
	managerMsgSealed()
}

// ManagerResp is the response type returned by the VTXO Manager actor.
type ManagerResp interface {
	managerRespSealed()
}

// VTXOCreatedMsg notifies the manager that new VTXOs have been created by a
// completed round. The manager spawns a new actor for each VTXO.
type VTXOCreatedMsg struct {
	actor.BaseMessage

	// VTXOs are the ClientVTXOs from the completed round.
	VTXOs []*round.ClientVTXO

	// RoundID identifies the round that created these VTXOs.
	RoundID string

	// CommitmentTxID is the txid of the commitment transaction.
	CommitmentTxID chainhash.Hash

	// BatchExpiry is the absolute block height when the batch expires.
	BatchExpiry int32

	// TreeDepth is the depth of VTXOs in the commitment tree.
	TreeDepth int

	// CreatedHeight is the block height when the VTXOs were created.
	CreatedHeight int32

	// TapScript is the tapscript for the VTXOs (shared across batch).
	TapScript *waddrmgr.Tapscript
}

func (m *VTXOCreatedMsg) managerMsgSealed() {}

// MessageType returns the message type for logging.
func (m *VTXOCreatedMsg) MessageType() string { return "VTXOCreatedMsg" }

// VTXOCreatedResp is the response to VTXOCreatedMsg.
type VTXOCreatedResp struct{}

func (r *VTXOCreatedResp) managerRespSealed() {}

// VTXOTerminatedMsg notifies the manager that a VTXO actor has reached a
// terminal state and should be removed from tracking.
type VTXOTerminatedMsg struct {
	actor.BaseMessage

	// Outpoint identifies the terminated VTXO.
	Outpoint wire.OutPoint

	// FinalState is the terminal state reached.
	FinalState string

	// Reason explains why the VTXO terminated.
	Reason string
}

func (m *VTXOTerminatedMsg) managerMsgSealed() {}

// MessageType returns the message type for logging.
func (m *VTXOTerminatedMsg) MessageType() string { return "VTXOTerminatedMsg" }

// VTXOTerminatedResp is the response to VTXOTerminatedMsg.
type VTXOTerminatedResp struct{}

func (r *VTXOTerminatedResp) managerRespSealed() {}

// GetActiveVTXOCountRequest requests the number of active VTXO actors managed
// by the VTXO Manager. This goes through the actor message path to avoid
// requiring synchronization.
type GetActiveVTXOCountRequest struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier.
func (r *GetActiveVTXOCountRequest) MessageType() string {
	return "GetActiveVTXOCountRequest"
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (r *GetActiveVTXOCountRequest) VTXOManagerMsg() {}

// GetActiveVTXOCountResponse returns the count of active VTXO actors.
type GetActiveVTXOCountResponse struct {
	// Count is the number of currently active VTXO actors.
	Count int
}

func (r *GetActiveVTXOCountResponse) managerRespSealed() {}
