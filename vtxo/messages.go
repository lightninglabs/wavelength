package vtxo

import (
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/round"
)

// ManagerMsg is the message type for the VTXO Manager actor. This is a type
// alias for actormsg.VTXOManagerMsg so the manager can be registered with
// the well-known VTXOManagerServiceKey and looked up by the wallet actor
// without import cycles.
type ManagerMsg = actormsg.VTXOManagerMsg

// ManagerResp is the response type for the VTXO Manager actor. This is a
// type alias for actormsg.VTXOManagerResp so responses from the manager
// can be received by the wallet actor via the service key lookup.
type ManagerResp = actormsg.VTXOManagerResp

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
