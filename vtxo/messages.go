package vtxo

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
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

// VTXOsMaterializedResp is the response to VTXOsMaterializedNotification.
type VTXOsMaterializedResp struct{}

// VTXOManagerResp implements actormsg.VTXOManagerResp marker interface.
func (r *VTXOsMaterializedResp) VTXOManagerResp() {}

// VTXOTerminatedResp is the response to VTXOTerminatedMsg.
type VTXOTerminatedResp struct{}

func (r *VTXOTerminatedResp) managerRespSealed() {}

// VTXOsMaterializedNotification notifies the VTXO manager that VTXOs were
// already durably persisted by another actor and only actor activation remains.
//
// The OOR receive path uses this after materializing incoming VTXOs so the
// manager can spawn one VTXO actor per descriptor without performing another
// store write.
type VTXOsMaterializedNotification struct {
	actor.BaseMessage

	// VTXOs are the descriptors that were already persisted locally.
	VTXOs []*Descriptor
}

// MessageType returns the message type identifier.
func (m *VTXOsMaterializedNotification) MessageType() string {
	return "VTXOsMaterializedNotification"
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (m *VTXOsMaterializedNotification) VTXOManagerMsg() {}

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

// =============================================================================
// Spend admission messages: wallet → Manager → VTXO actors
// =============================================================================

// SelectAndReserveSpendRequest asks the manager to select VTXOs covering a
// target amount and atomically reserve them for an OOR spend. The manager
// runs largest-first coin selection, then Asks each selected VTXO actor to
// process SpendReserveEvent. If any reservation fails, already-reserved
// VTXOs are rolled back.
type SelectAndReserveSpendRequest struct {
	actor.BaseMessage

	// TargetAmount is the minimum total value the selected VTXOs must
	// cover.
	TargetAmount btcutil.Amount
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (m *SelectAndReserveSpendRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *SelectAndReserveSpendRequest) MessageType() string {
	return "SelectAndReserveSpendRequest"
}

// SelectedVTXO describes a VTXO that was selected and reserved for an OOR
// spend. This is returned in the SelectAndReserveSpendResponse.
type SelectedVTXO struct {
	// Outpoint is the selected VTXO's outpoint.
	Outpoint wire.OutPoint

	// Amount is the value of this VTXO in satoshis.
	Amount btcutil.Amount

	// PkScript is the output script for this VTXO.
	PkScript []byte
}

// SelectAndReserveSpendResponse returns the VTXOs that were selected and
// reserved for an OOR spend.
type SelectAndReserveSpendResponse struct {
	// SelectedVTXOs is the set of VTXOs reserved for this spend.
	SelectedVTXOs []SelectedVTXO

	// TotalSelected is the sum of all selected VTXO amounts.
	TotalSelected btcutil.Amount
}

// managerRespSealed implements the ManagerResp sealed interface.
func (r *SelectAndReserveSpendResponse) managerRespSealed() {}

// ReleaseSpendRequest releases VTXOs previously reserved for an OOR spend
// back to LiveState. Used when the OOR operation fails or is cancelled.
type ReleaseSpendRequest struct {
	actor.BaseMessage

	// Outpoints identifies the VTXOs to release from spend reservation.
	Outpoints []wire.OutPoint
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (m *ReleaseSpendRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *ReleaseSpendRequest) MessageType() string {
	return "ReleaseSpendRequest"
}

// ReleaseSpendResponse confirms the spend release.
type ReleaseSpendResponse struct {
	// ReleasedCount is the number of VTXOs successfully released.
	ReleasedCount int
}

// managerRespSealed implements the ManagerResp sealed interface.
func (r *ReleaseSpendResponse) managerRespSealed() {}

// CompleteSpendRequest marks VTXOs as fully spent via an OOR transaction.
// This transitions each VTXO from SpendingState to terminal SpentState.
type CompleteSpendRequest struct {
	actor.BaseMessage

	// Outpoints identifies the VTXOs to mark as spent.
	Outpoints []wire.OutPoint
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (m *CompleteSpendRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *CompleteSpendRequest) MessageType() string {
	return "CompleteSpendRequest"
}

// CompleteSpendResponse confirms the spend completion.
type CompleteSpendResponse struct {
	// CompletedCount is the number of VTXOs marked as spent.
	CompletedCount int
}

// managerRespSealed implements the ManagerResp sealed interface.
func (r *CompleteSpendResponse) managerRespSealed() {}

// =============================================================================
// Forfeit admission messages: wallet → Manager → VTXO actors
// =============================================================================

// ReserveForfeitRequest asks the manager to reserve specific VTXOs for
// cooperative consumption. The manager Asks each actor to process
// PendingForfeitEvent. If any reservation fails, already-claimed VTXOs
// are rolled back via ForfeitReleasedEvent.
type ReserveForfeitRequest struct {
	actor.BaseMessage

	// Outpoints identifies the VTXOs to reserve for forfeit.
	Outpoints []wire.OutPoint
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (m *ReserveForfeitRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *ReserveForfeitRequest) MessageType() string {
	return "ReserveForfeitRequest"
}

// ReserveForfeitResponse confirms the forfeit reservation.
type ReserveForfeitResponse struct{}

// managerRespSealed implements the ManagerResp sealed interface.
func (r *ReserveForfeitResponse) managerRespSealed() {}

// ReleaseForfeitRequest releases VTXOs from pending forfeit back to
// LiveState. Used when round registration fails after admission.
type ReleaseForfeitRequest struct {
	actor.BaseMessage

	// Outpoints identifies the VTXOs to release from forfeit.
	Outpoints []wire.OutPoint
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (m *ReleaseForfeitRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *ReleaseForfeitRequest) MessageType() string {
	return "ReleaseForfeitRequest"
}

// ReleaseForfeitResponse confirms the forfeit release.
type ReleaseForfeitResponse struct {
	// ReleasedCount is the number of VTXOs released.
	ReleasedCount int
}

// managerRespSealed implements the ManagerResp sealed interface.
func (r *ReleaseForfeitResponse) managerRespSealed() {}
