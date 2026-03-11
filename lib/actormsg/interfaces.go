package actormsg

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/types"
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

// RoundActorResp is the response type marker for round actors. The concrete
// round.ClientResp struct implements this interface. Using an interface here
// allows the wallet to look up the round actor without importing the round
// package (avoiding import cycles).
type RoundActorResp interface {
	RoundActorResp()
}

// VTXOManagerMsg is the message type for VTXO manager. Messages sent TO the
// manager implement this interface via the exported marker method.
type VTXOManagerMsg interface {
	actor.Message
	VTXOManagerMsg()
}

// VTXOManagerResp is the response type marker for the VTXO manager. The
// concrete vtxo.ManagerResp interface embeds this marker, enabling service
// key lookup from the wallet package without importing the vtxo package
// (avoiding import cycles).
type VTXOManagerResp interface {
	VTXOManagerResp()
}

// RegisterIntentMsg is sent from the wallet actor to the round actor to
// register a pre-composed intent package. The wallet builds the full set of
// forfeits, VTXO requests, and leave requests; the round actor validates
// and registers it with the FSM.
//
// Defined in actormsg to avoid the wallet→round import cycle.
type RegisterIntentMsg struct {
	actor.BaseMessage

	// Forfeits contains the VTXOs being forfeited as inputs.
	Forfeits []types.ForfeitRequest

	// VTXOs is the templates for the VTXO(s) requested in the round.
	VTXOs []types.VTXORequest

	// Leaves contains the leave requests for VTXOs being exited to
	// on-chain outputs.
	Leaves []*types.LeaveRequest
}

// RoundReceivable implements the RoundReceivable marker interface.
func (m *RegisterIntentMsg) RoundReceivable() {}

// MessageType returns the message type for logging.
func (m *RegisterIntentMsg) MessageType() string {
	return "RegisterIntentMsg"
}

// TriggerBoardMsg is sent from the wallet actor to the round actor
// to trigger boarding of confirmed UTXOs into the next round. The
// wallet computes the VTXO output amounts after deducting operator
// fees, then delegates round registration to the round actor.
// Defined in actormsg to avoid import cycle between wallet and
// round packages.
type TriggerBoardMsg struct {
	actor.BaseMessage

	// Amounts contains the VTXO output amounts to register for the next
	// round. Typically a single amount equal to the confirmed boarding
	// balance minus the operator fee.
	Amounts []btcutil.Amount
}

// RoundReceivable implements the RoundReceivable marker interface.
func (m *TriggerBoardMsg) RoundReceivable() {}

// MessageType returns the message type for logging.
func (m *TriggerBoardMsg) MessageType() string {
	return "TriggerBoardMsg"
}
