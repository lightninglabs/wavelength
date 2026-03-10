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

// VTXOManagerResp is the response type marker for the VTXO manager actor.
// Using an interface here allows the wallet to look up and Ask the VTXO
// manager without importing the vtxo package (avoiding import cycles).
type VTXOManagerResp interface {
	VTXOManagerResp()
}

// AvailableVTXO describes a live, unlocked VTXO available for coin selection.
// This lightweight type avoids requiring callers to import the vtxo package.
type AvailableVTXO struct {
	// Outpoint is the VTXO's outpoint.
	Outpoint wire.OutPoint

	// Amount is the value of this VTXO in satoshis.
	Amount int64

	// PkScript is the output script for this VTXO.
	PkScript []byte
}

// ListAvailableVTXOsRequest asks the VTXO manager for all live VTXOs that
// are not currently locked. The wallet actor uses this to run coin selection
// against the set of spendable VTXOs.
type ListAvailableVTXOsRequest struct {
	actor.BaseMessage
}

// VTXOManagerMsg implements the VTXOManagerMsg marker interface.
func (m *ListAvailableVTXOsRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *ListAvailableVTXOsRequest) MessageType() string {
	return "ListAvailableVTXOsRequest"
}

// ListAvailableVTXOsResponse returns the set of live, unlocked VTXOs.
type ListAvailableVTXOsResponse struct {
	// Available is the set of VTXOs that are live and not locked.
	Available []AvailableVTXO
}

// VTXOManagerResp implements the VTXOManagerResp marker interface.
func (m *ListAvailableVTXOsResponse) VTXOManagerResp() {}

// SelectAndLockVTXOsRequest asks the VTXO manager to atomically select and
// lock a set of live VTXOs that cover the target amount. This avoids a race
// between separate list and lock requests.
type SelectAndLockVTXOsRequest struct {
	actor.BaseMessage

	// TargetAmount is the minimum total value the selected VTXOs must
	// cover, in satoshis.
	TargetAmount int64
}

// VTXOManagerMsg implements the VTXOManagerMsg marker interface.
func (m *SelectAndLockVTXOsRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *SelectAndLockVTXOsRequest) MessageType() string {
	return "SelectAndLockVTXOsRequest"
}

// SelectAndLockVTXOsResponse returns the VTXOs that were selected and locked
// by the manager.
type SelectAndLockVTXOsResponse struct {
	// Selected is the set of VTXOs that were selected and locked.
	Selected []AvailableVTXO

	// TotalSelected is the sum of all selected VTXO amounts in satoshis.
	TotalSelected int64
}

// VTXOManagerResp implements the VTXOManagerResp marker interface.
func (m *SelectAndLockVTXOsResponse) VTXOManagerResp() {}

// LockVTXOsRequest asks the VTXO manager to mark the given outpoints as
// locked. Locked VTXOs are excluded from future ListAvailableVTXOs results
// until explicitly unlocked.
type LockVTXOsRequest struct {
	actor.BaseMessage

	// Outpoints identifies the VTXOs to lock.
	Outpoints []wire.OutPoint
}

// VTXOManagerMsg implements the VTXOManagerMsg marker interface.
func (m *LockVTXOsRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *LockVTXOsRequest) MessageType() string {
	return "LockVTXOsRequest"
}

// LockVTXOsResponse confirms that the specified VTXOs were locked.
type LockVTXOsResponse struct {
	// LockedCount is the number of VTXOs that were newly locked.
	LockedCount int
}

// VTXOManagerResp implements the VTXOManagerResp marker interface.
func (m *LockVTXOsResponse) VTXOManagerResp() {}

// UnlockVTXOsRequest asks the VTXO manager to release locks on the given
// outpoints. This is used when a transfer or round participation fails or
// is cancelled, allowing the VTXOs to be selected again.
type UnlockVTXOsRequest struct {
	actor.BaseMessage

	// Outpoints identifies the VTXOs to unlock.
	Outpoints []wire.OutPoint
}

// VTXOManagerMsg implements the VTXOManagerMsg marker interface.
func (m *UnlockVTXOsRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *UnlockVTXOsRequest) MessageType() string {
	return "UnlockVTXOsRequest"
}

// UnlockVTXOsResponse confirms that the specified VTXOs were unlocked.
type UnlockVTXOsResponse struct {
	// UnlockedCount is the number of VTXOs that were actually unlocked.
	UnlockedCount int
}

// VTXOManagerResp implements the VTXOManagerResp marker interface.
func (m *UnlockVTXOsResponse) VTXOManagerResp() {}

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

// TriggerVTXOLeaveMsg is sent from the wallet actor to the round actor to
// request leave (offboard) of specific VTXOs. The VTXOs will be forfeited and
// their value sent to the specified destination output.
type TriggerVTXOLeaveMsg struct {
	actor.BaseMessage

	// TargetOutpoints specifies which VTXOs to leave (offboard).
	TargetOutpoints []wire.OutPoint

	// DestOutput is the on-chain destination output where the funds will
	// be sent. This output will be included in the batch transaction.
	DestOutput *wire.TxOut
}

// RoundReceivable implements the RoundReceivable marker interface.
func (m *TriggerVTXOLeaveMsg) RoundReceivable() {}

// MessageType returns the message type for logging.
func (m *TriggerVTXOLeaveMsg) MessageType() string {
	return "TriggerVTXOLeaveMsg"
}
