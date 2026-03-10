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

// ManagerResp embeds actormsg.VTXOManagerResp for responses returned by the
// VTXO Manager actor. Response types defined in actormsg (for cross-package
// use) and in this package both satisfy this interface via the exported
// VTXOManagerResp() marker method.
type ManagerResp interface {
	actormsg.VTXOManagerResp
}

// Type alias for VTXOTerminatedMsg - canonical definition is in round package.
type VTXOTerminatedMsg = round.VTXOTerminatedMsg

// VTXOCreatedResp is the response to VTXOCreatedNotification.
type VTXOCreatedResp struct{}

// VTXOManagerResp implements the actormsg.VTXOManagerResp marker interface.
func (r *VTXOCreatedResp) VTXOManagerResp() {}

// VTXOTerminatedResp is the response to VTXOTerminatedMsg.
type VTXOTerminatedResp struct{}

// VTXOManagerResp implements the actormsg.VTXOManagerResp marker interface.
func (r *VTXOTerminatedResp) VTXOManagerResp() {}

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

// VTXOManagerResp implements the actormsg.VTXOManagerResp marker interface.
func (r *GetActiveVTXOCountResponse) VTXOManagerResp() {}
