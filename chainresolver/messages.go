package chainresolver

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/tree"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ClientResolverKey is the service key for the ClientResolverActor. This key
// is used to spawn and discover ClientResolverActor instances within an actor
// system.
var ClientResolverKey = actor.NewServiceKey[
	ClientResolverMsg, ClientResolverResp,
]("client-resolver")

// ClientResolverMsg is the sealed interface for all messages that can be sent
// to the ClientResolverActor. The sealed interface pattern ensures type safety
// by preventing external packages from implementing the interface.
type ClientResolverMsg interface {
	actor.Message
	clientResolverMsgSealed()
}

// ClientResolverResp is the sealed interface for all response messages from
// the ClientResolverActor.
type ClientResolverResp interface {
	actor.Message
	clientResolverRespSealed()
}

// ClientResolverEvent is the sealed interface for all event notifications
// that can be sent from the ClientResolverActor to registered actors.
type ClientResolverEvent interface {
	actor.Message
	clientResolverEventSealed()
}

// -----------------------------------------------------------------------------
// VTXO Monitoring Messages
// -----------------------------------------------------------------------------

// MonitorVTXORequest registers a VTXO for monitoring. The client resolver will
// track confirmations, detect spends, and notify the caller of relevant events.
type MonitorVTXORequest struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the VTXO output to monitor.
	VTXOOutpoint wire.OutPoint

	// VTXOOutput is the TxOut at VTXOOutpoint.
	VTXOOutput *wire.TxOut

	// TreePath is the user's extracted tree path from root to this VTXO.
	// Used for unroll initiation and path validation.
	TreePath *tree.Tree

	// ExitDelay is the CSV timeout in blocks for unilateral recovery.
	ExitDelay uint32

	// HeightHint is the block height hint for faster chain scanning.
	HeightHint uint32

	// NotifyActor is an optional actor reference to receive events.
	NotifyActor fn.Option[actor.TellOnlyRef[ClientResolverEvent]]
}

// MessageType returns the message type identifier.
func (m *MonitorVTXORequest) MessageType() string {
	return "MonitorVTXORequest"
}

// clientResolverMsgSealed implements the sealed interface.
func (m *MonitorVTXORequest) clientResolverMsgSealed() {}

// MonitorVTXOResponse confirms that VTXO monitoring has been initiated.
type MonitorVTXOResponse struct {
	actor.BaseMessage

	// MonitorID is a unique identifier for this monitoring subscription.
	MonitorID string
}

// MessageType returns the message type identifier.
func (m *MonitorVTXOResponse) MessageType() string {
	return "MonitorVTXOResponse"
}

// clientResolverRespSealed implements the sealed interface.
func (m *MonitorVTXOResponse) clientResolverRespSealed() {}

// StopMonitorVTXORequest cancels monitoring for a specific VTXO.
type StopMonitorVTXORequest struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the VTXO to stop monitoring.
	VTXOOutpoint wire.OutPoint
}

// MessageType returns the message type identifier.
func (m *StopMonitorVTXORequest) MessageType() string {
	return "StopMonitorVTXORequest"
}

// clientResolverMsgSealed implements the sealed interface.
func (m *StopMonitorVTXORequest) clientResolverMsgSealed() {}

// StopMonitorVTXOResponse confirms that monitoring has been stopped.
type StopMonitorVTXOResponse struct {
	actor.BaseMessage

	// Stopped indicates whether monitoring was stopped.
	Stopped bool
}

// MessageType returns the message type identifier.
func (m *StopMonitorVTXOResponse) MessageType() string {
	return "StopMonitorVTXOResponse"
}

// clientResolverRespSealed implements the sealed interface.
func (m *StopMonitorVTXOResponse) clientResolverRespSealed() {}

// -----------------------------------------------------------------------------
// Unroll Initiation Messages
// -----------------------------------------------------------------------------

// InitiateUnrollRequest broadcasts the VTXT path to claim a VTXO on-chain.
// This is used when the user wants to exit the Ark and claim their funds.
type InitiateUnrollRequest struct {
	actor.BaseMessage

	// TreePath is the user's signed tree path from root to their VTXO.
	TreePath *tree.Tree

	// CoSignerKey is the user's public key in the tree.
	CoSignerKey *btcec.PublicKey
}

// MessageType returns the message type identifier.
func (m *InitiateUnrollRequest) MessageType() string {
	return "InitiateUnrollRequest"
}

// clientResolverMsgSealed implements the sealed interface.
func (m *InitiateUnrollRequest) clientResolverMsgSealed() {}

// InitiateUnrollResponse contains the result of an unroll initiation.
type InitiateUnrollResponse struct {
	actor.BaseMessage

	// BroadcastTxids contains the transaction IDs of all transactions
	// broadcast as part of the unroll, from root to leaf.
	BroadcastTxids []chainhash.Hash

	// LeafOutpoint is the final VTXO output that can now be claimed.
	LeafOutpoint wire.OutPoint
}

// MessageType returns the message type identifier.
func (m *InitiateUnrollResponse) MessageType() string {
	return "InitiateUnrollResponse"
}

// clientResolverRespSealed implements the sealed interface.
func (m *InitiateUnrollResponse) clientResolverRespSealed() {}

// -----------------------------------------------------------------------------
// CSV Timeout Recovery Messages
// -----------------------------------------------------------------------------

// RecoverVTXORequest unilaterally sweeps a VTXO via the CSV timeout path.
// This is used after the exit delay has elapsed and the user wants to
// recover funds without operator cooperation.
type RecoverVTXORequest struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the VTXO to recover.
	VTXOOutpoint wire.OutPoint

	// VTXOOutput is the TxOut at VTXOOutpoint.
	VTXOOutput *wire.TxOut

	// CSVTimeout is the CSV timeout value that must have elapsed.
	CSVTimeout uint32

	// Destination is the address to send recovered funds to.
	Destination btcutil.Address
}

// MessageType returns the message type identifier.
func (m *RecoverVTXORequest) MessageType() string {
	return "RecoverVTXORequest"
}

// clientResolverMsgSealed implements the sealed interface.
func (m *RecoverVTXORequest) clientResolverMsgSealed() {}

// RecoverVTXOResponse contains the result of a VTXO recovery.
type RecoverVTXOResponse struct {
	actor.BaseMessage

	// RecoveryTxid is the transaction ID of the recovery transaction.
	RecoveryTxid chainhash.Hash

	// Amount is the value recovered (before fees).
	Amount btcutil.Amount
}

// MessageType returns the message type identifier.
func (m *RecoverVTXOResponse) MessageType() string {
	return "RecoverVTXOResponse"
}

// clientResolverRespSealed implements the sealed interface.
func (m *RecoverVTXOResponse) clientResolverRespSealed() {}

// -----------------------------------------------------------------------------
// Boarding UTXO Monitoring Messages
// -----------------------------------------------------------------------------

// MonitorBoardingRequest tracks a boarding address for incoming deposits.
type MonitorBoardingRequest struct {
	actor.BaseMessage

	// BoardingAddress is the address to monitor.
	BoardingAddress btcutil.Address

	// PkScript is the scriptPubKey of the boarding address.
	PkScript []byte

	// ExitDelay is the CSV timeout for the boarding output.
	ExitDelay uint32

	// NotifyActor is an optional actor reference to receive events.
	NotifyActor fn.Option[actor.TellOnlyRef[ClientResolverEvent]]
}

// MessageType returns the message type identifier.
func (m *MonitorBoardingRequest) MessageType() string {
	return "MonitorBoardingRequest"
}

// clientResolverMsgSealed implements the sealed interface.
func (m *MonitorBoardingRequest) clientResolverMsgSealed() {}

// MonitorBoardingResponse confirms that boarding monitoring has started.
type MonitorBoardingResponse struct {
	actor.BaseMessage

	// MonitorID is a unique identifier for this monitoring subscription.
	MonitorID string
}

// MessageType returns the message type identifier.
func (m *MonitorBoardingResponse) MessageType() string {
	return "MonitorBoardingResponse"
}

// clientResolverRespSealed implements the sealed interface.
func (m *MonitorBoardingResponse) clientResolverRespSealed() {}

// -----------------------------------------------------------------------------
// Event Notifications
// -----------------------------------------------------------------------------

// VTXOSpentEvent is sent when a monitored VTXO is spent.
type VTXOSpentEvent struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the spent VTXO.
	VTXOOutpoint wire.OutPoint

	// SpendingTx is the transaction that spent the VTXO.
	SpendingTx *wire.MsgTx

	// SpendingHeight is the block height where the spend was confirmed.
	SpendingHeight int32

	// ExpectedSpend is true if the user initiated this spend (unroll),
	// false if it was unexpected (potential fraud by server).
	ExpectedSpend bool
}

// MessageType returns the message type identifier.
func (m VTXOSpentEvent) MessageType() string {
	return "VTXOSpentEvent"
}

// clientResolverEventSealed implements the sealed interface.
func (m VTXOSpentEvent) clientResolverEventSealed() {}

// VTXOConfirmedEvent is sent when an unroll completes and the VTXO is
// confirmed on-chain.
type VTXOConfirmedEvent struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the confirmed VTXO.
	VTXOOutpoint wire.OutPoint

	// Confirmations is the number of confirmations.
	Confirmations int32
}

// MessageType returns the message type identifier.
func (m VTXOConfirmedEvent) MessageType() string {
	return "VTXOConfirmedEvent"
}

// clientResolverEventSealed implements the sealed interface.
func (m VTXOConfirmedEvent) clientResolverEventSealed() {}

// CSVTimeoutReachedEvent is sent when the CSV timeout has elapsed and
// unilateral recovery is now possible.
type CSVTimeoutReachedEvent struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the VTXO that can now be recovered.
	VTXOOutpoint wire.OutPoint

	// TimeoutHeight is the block height at which timeout was reached.
	TimeoutHeight int32
}

// MessageType returns the message type identifier.
func (m CSVTimeoutReachedEvent) MessageType() string {
	return "CSVTimeoutReachedEvent"
}

// clientResolverEventSealed implements the sealed interface.
func (m CSVTimeoutReachedEvent) clientResolverEventSealed() {}

// BoardingDepositEvent is sent when funds are received at a boarding address.
type BoardingDepositEvent struct {
	actor.BaseMessage

	// BoardingAddress is the address that received funds.
	BoardingAddress btcutil.Address

	// Outpoint identifies the deposit output.
	Outpoint wire.OutPoint

	// Amount is the value of the deposit.
	Amount btcutil.Amount

	// Confirmations is the number of confirmations.
	Confirmations int32
}

// MessageType returns the message type identifier.
func (m BoardingDepositEvent) MessageType() string {
	return "BoardingDepositEvent"
}

// clientResolverEventSealed implements the sealed interface.
func (m BoardingDepositEvent) clientResolverEventSealed() {}

// -----------------------------------------------------------------------------
// Internal Messages (for communication between actors)
// -----------------------------------------------------------------------------

// vtxoMonitorMsg is the sealed interface for messages to VTXOMonitorActor.
type vtxoMonitorMsg interface {
	actor.Message
	vtxoMonitorMsgSealed()
}

// vtxoMonitorResp is the sealed interface for responses from VTXOMonitorActor.
type vtxoMonitorResp interface {
	actor.Message
	vtxoMonitorRespSealed()
}

// startVTXOMonitorRequest is the initial message to start VTXO monitoring.
type startVTXOMonitorRequest struct {
	actor.BaseMessage

	// Config contains the monitoring configuration.
	Config *MonitorVTXORequest

	// Parent is a reference to the ClientResolverActor.
	Parent actor.TellOnlyRef[ClientResolverMsg]

	// SelfRef is the VTXOMonitorActor's own reference for callbacks.
	SelfRef actor.TellOnlyRef[vtxoMonitorMsg]
}

// MessageType returns the message type identifier.
func (m *startVTXOMonitorRequest) MessageType() string {
	return "startVTXOMonitorRequest"
}

// vtxoMonitorMsgSealed implements the sealed interface.
func (m *startVTXOMonitorRequest) vtxoMonitorMsgSealed() {}

// startVTXOMonitorResponse confirms monitoring has started.
type startVTXOMonitorResponse struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier.
func (m *startVTXOMonitorResponse) MessageType() string {
	return "startVTXOMonitorResponse"
}

// vtxoMonitorRespSealed implements the sealed interface.
func (m *startVTXOMonitorResponse) vtxoMonitorRespSealed() {}

// stopVTXOMonitorRequest stops VTXO monitoring.
type stopVTXOMonitorRequest struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier.
func (m *stopVTXOMonitorRequest) MessageType() string {
	return "stopVTXOMonitorRequest"
}

// vtxoMonitorMsgSealed implements the sealed interface.
func (m *stopVTXOMonitorRequest) vtxoMonitorMsgSealed() {}

// stopVTXOMonitorResponse confirms monitoring has stopped.
type stopVTXOMonitorResponse struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier.
func (m *stopVTXOMonitorResponse) MessageType() string {
	return "stopVTXOMonitorResponse"
}

// vtxoMonitorRespSealed implements the sealed interface.
func (m *stopVTXOMonitorResponse) vtxoMonitorRespSealed() {}

// internalVTXOSpendEvent wraps spend events from chainsource.
type internalVTXOSpendEvent struct {
	actor.BaseMessage

	// Event contains the spend event from chainsource.
	Event chainsource.SpendEvent
}

// MessageType returns the message type identifier.
func (m *internalVTXOSpendEvent) MessageType() string {
	return "internalVTXOSpendEvent"
}

// vtxoMonitorMsgSealed implements the sealed interface.
func (m *internalVTXOSpendEvent) vtxoMonitorMsgSealed() {}

// internalVTXOBlockEpoch wraps block epoch events from chainsource.
type internalVTXOBlockEpoch struct {
	actor.BaseMessage

	// Epoch contains the block epoch from chainsource.
	Epoch chainsource.BlockEpoch
}

// MessageType returns the message type identifier.
func (m *internalVTXOBlockEpoch) MessageType() string {
	return "internalVTXOBlockEpoch"
}

// vtxoMonitorMsgSealed implements the sealed interface.
func (m *internalVTXOBlockEpoch) vtxoMonitorMsgSealed() {}

// internalVTXOSpentNotification is sent from VTXOMonitorActor to parent.
type internalVTXOSpentNotification struct {
	actor.BaseMessage

	// Event contains the spend details.
	Event VTXOSpentEvent
}

// MessageType returns the message type identifier.
func (m *internalVTXOSpentNotification) MessageType() string {
	return "internalVTXOSpentNotification"
}

// clientResolverMsgSealed implements the sealed interface.
func (m *internalVTXOSpentNotification) clientResolverMsgSealed() {}

// internalCSVTimeoutNotification is sent when CSV timeout is reached.
type internalCSVTimeoutNotification struct {
	actor.BaseMessage

	// Event contains the timeout details.
	Event CSVTimeoutReachedEvent
}

// MessageType returns the message type identifier.
func (m *internalCSVTimeoutNotification) MessageType() string {
	return "internalCSVTimeoutNotification"
}

// clientResolverMsgSealed implements the sealed interface.
func (m *internalCSVTimeoutNotification) clientResolverMsgSealed() {}
