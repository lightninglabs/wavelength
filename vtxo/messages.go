package vtxo

import (
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/round"
)

// ManagerMsg embeds actormsg.VTXOManagerMsg for messages accepted by the VTXO
// Manager actor. Message types are defined in round/vtxo_messages.go and
// implement the actormsg.VTXOManagerMsg marker interface.
type ManagerMsg interface {
	actormsg.VTXOManagerMsg
}

// ManagerResp is the response type returned by the VTXO Manager actor.
type ManagerResp interface {
	managerRespSealed()
}

// Type alias for VTXOTerminatedMsg - canonical definition is in round package.
type VTXOTerminatedMsg = round.VTXOTerminatedMsg

// VTXOCreatedResp is the response to VTXOCreatedNotification.
type VTXOCreatedResp struct{}

func (r *VTXOCreatedResp) managerRespSealed() {}

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

// =============================================================================
// Relay messages: VTXO actor → Manager → external actor
// =============================================================================
//
// The VTXO actor routes all outbound signals through the manager rather than
// holding direct references to the round actor or chain resolver. These relay
// messages carry pre-built payloads that the manager unwraps and forwards.

// RelayToRoundMsg wraps a message that the manager should relay to the round
// actor. The payload is already in the round-receivable format so the manager
// just forwards it without transformation.
type RelayToRoundMsg struct {
	actor.BaseMessage

	// Payload is the round-receivable message to relay.
	Payload actormsg.RoundReceivable
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (m *RelayToRoundMsg) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *RelayToRoundMsg) MessageType() string { return "RelayToRoundMsg" }

// RelayToRoundResp is the response for RelayToRoundMsg.
type RelayToRoundResp struct{}

// managerRespSealed implements the ManagerResp sealed interface.
func (r *RelayToRoundResp) managerRespSealed() {}

