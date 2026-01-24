package actormsg

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// VTXOActorMsg is the message type for VTXO actors. Messages sent TO VTXO
// actors implement this interface via the exported marker method. This enables
// both round and vtxo packages to use consistent types without import cycles.
type VTXOActorMsg interface {
	actor.Message
	VTXOActorMsg()
}

// VTXOActorResp is the response type marker for VTXO actors. The concrete
// vtxo.VTXOActorResponse struct implements this interface. Using an interface
// here allows both packages to use the same service key type parameters.
type VTXOActorResp interface {
	VTXOActorResp()
}

// RoundReceivable marks messages that can be received by the round actor.
// round.ClientMsg embeds this interface. Both round-internal messages and
// messages from other actors (vtxo, wallet) implement this marker.
type RoundReceivable interface {
	actor.Message
	RoundReceivable()
}

// VTXOManagerMsg is the message type for VTXO manager. Messages sent TO the
// manager implement this interface via the exported marker method.
type VTXOManagerMsg interface {
	actor.Message
	VTXOManagerMsg()
}

// TriggerVTXORefreshMsg is sent from the wallet actor to the round actor to
// request refresh of specific VTXOs. Defined in actormsg to avoid import cycle
// between wallet and round packages.
type TriggerVTXORefreshMsg struct {
	actor.BaseMessage

	// TargetOutpoints specifies which VTXOs to refresh.
	TargetOutpoints []wire.OutPoint

	// ForceRefresh indicates this is a user-initiated refresh that should
	// proceed regardless of expiry status.
	ForceRefresh bool
}

// RoundReceivable implements the RoundReceivable marker interface.
func (m *TriggerVTXORefreshMsg) RoundReceivable() {}

// MessageType returns the message type for logging.
func (m *TriggerVTXORefreshMsg) MessageType() string {
	return "TriggerVTXORefreshMsg"
}
