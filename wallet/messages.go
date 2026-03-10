package wallet

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// WalletMsg is the sealed interface for all messages that can be sent to the
// Boarding Wallet actor. The sealed interface pattern ensures type safety by
// preventing external packages from implementing the interface.
type WalletMsg interface {
	actor.Message
	walletMsgSealed()
}

// WalletResp is the sealed interface for all response messages from the
// Boarding Wallet actor.
type WalletResp interface {
	actor.Message
	walletRespSealed()
}

// CreateBoardingAddressRequest requests the creation of a new boarding
// address. The wallet actor will derive a new client key, construct a 2-of-2
// tapscript with the operator key and CSV timelock, import it into LND, and
// return the address.
type CreateBoardingAddressRequest struct {
	actor.BaseMessage

	// OperatorKey is the operator's public key for the 2-of-2 tapscript
	// collaborative spend path.
	OperatorKey *btcec.PublicKey

	// ExitDelay is the CSV delay in blocks for the client's unilateral
	// exit path. Must meet the operator's minimum boarding exit delay.
	ExitDelay uint32
}

// MessageType returns the message type identifier for logging and debugging.
func (m *CreateBoardingAddressRequest) MessageType() string {
	return "CreateBoardingAddressRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *CreateBoardingAddressRequest) walletMsgSealed() {}

// CreateBoardingAddressResponse contains the newly created boarding address
// and associated metadata.
type CreateBoardingAddressResponse struct {
	actor.BaseMessage

	// Address is the boarding address that users can send funds to.
	Address btcutil.Address

	// ClientKey is the derived client key used in the tapscript.
	ClientKey *btcec.PublicKey
}

// MessageType returns the message type identifier for logging and debugging.
func (m *CreateBoardingAddressResponse) MessageType() string {
	return "CreateBoardingAddressResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *CreateBoardingAddressResponse) walletRespSealed() {}

// GetActiveBoardingAddressesRequest requests a list of all boarding addresses
// that have been created and are actively monitored.
type GetActiveBoardingAddressesRequest struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier for logging and debugging.
func (m *GetActiveBoardingAddressesRequest) MessageType() string {
	return "GetActiveBoardingAddressesRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *GetActiveBoardingAddressesRequest) walletMsgSealed() {}

// GetActiveBoardingAddressesResponse contains the list of active boarding
// addresses.
type GetActiveBoardingAddressesResponse struct {
	actor.BaseMessage

	// Addresses is the list of all boarding addresses that have been
	// created and imported into the wallet.
	Addresses []*BoardingAddress
}

// MessageType returns the message type identifier for logging and debugging.
func (m *GetActiveBoardingAddressesResponse) MessageType() string {
	return "GetActiveBoardingAddressesResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *GetActiveBoardingAddressesResponse) walletRespSealed() {}

// GetBoardingBalanceRequest requests the total balance of all boarding UTXOs.
// This can be filtered to only include confirmed UTXOs or all detected UTXOs.
type GetBoardingBalanceRequest struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier for logging and debugging.
func (m *GetBoardingBalanceRequest) MessageType() string {
	return "GetBoardingBalanceRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *GetBoardingBalanceRequest) walletMsgSealed() {}

// GetBoardingBalanceResponse contains the total boarding balance and UTXO
// count.
type GetBoardingBalanceResponse struct {
	actor.BaseMessage

	// TotalBalance is the sum of all matching boarding UTXOs.
	TotalBalance btcutil.Amount

	// UtxoCount is the number of UTXOs included in the balance.
	UtxoCount int
}

// MessageType returns the message type identifier for logging and debugging.
func (m *GetBoardingBalanceResponse) MessageType() string {
	return "GetBoardingBalanceResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *GetBoardingBalanceResponse) walletRespSealed() {}

// RegisterConfirmationNotifierRequest registers an actor to receive
// BoardingUtxoConfirmedEvent messages when new boarding UTXOs are detected and
// confirmed. This enables the round actor to be notified of new boarding
// opportunities.
type RegisterConfirmationNotifierRequest struct {
	actor.BaseMessage

	// NotifierID uniquely identifies this notifier for later
	// unregistration. Typically this is the actor's name or service key.
	NotifierID string

	// NotifyActor is the actor reference to send
	// BoardingUtxoConfirmedEvent messages to. Uses TellOnlyRef for
	// fire-and-forget delivery.
	NotifyActor actor.TellOnlyRef[BoardingUtxoConfirmedEvent]

	// BacklogHeight when set filters the backlog to only UTXOs confirmed
	// at or after this height. This allows actors to resume from a known
	// checkpoint height and avoid duplicate processing.
	BacklogHeight fn.Option[int32]

	// MinConf when set overrides the default minimum confirmation count
	// required before notifying this actor about a boarding UTXO. If not
	// specified, defaults to MinBoardingConfs.
	MinConf fn.Option[uint32]
}

// MessageType returns the message type identifier for logging and debugging.
func (m *RegisterConfirmationNotifierRequest) MessageType() string {
	return "RegisterConfirmationNotifierRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *RegisterConfirmationNotifierRequest) walletMsgSealed() {}

// RegisterConfirmationNotifierResponse indicates whether the registration
// succeeded.
type RegisterConfirmationNotifierResponse struct {
	actor.BaseMessage

	// Success indicates whether the notifier was successfully registered.
	Success bool
}

// MessageType returns the message type identifier for logging and debugging.
func (m *RegisterConfirmationNotifierResponse) MessageType() string {
	return "RegisterConfirmationNotifierResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *RegisterConfirmationNotifierResponse) walletRespSealed() {}

// UnregisterConfirmationNotifierRequest removes a previously registered
// confirmation notifier. The actor will no longer receive boarding UTXO
// confirmation events.
type UnregisterConfirmationNotifierRequest struct {
	actor.BaseMessage

	// NotifierID identifies the notifier to remove. Must match the ID
	// provided during registration.
	NotifierID string
}

// MessageType returns the message type identifier for logging and debugging.
func (m *UnregisterConfirmationNotifierRequest) MessageType() string {
	return "UnregisterConfirmationNotifierRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *UnregisterConfirmationNotifierRequest) walletMsgSealed() {}

// UnregisterConfirmationNotifierResponse indicates whether the unregistration
// succeeded.
type UnregisterConfirmationNotifierResponse struct {
	actor.BaseMessage

	// Success indicates whether the notifier was successfully unregistered.
	// Returns false if the notifier ID was not found.
	Success bool
}

// MessageType returns the message type identifier for logging and debugging.
func (m *UnregisterConfirmationNotifierResponse) MessageType() string {
	return "UnregisterConfirmationNotifierResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *UnregisterConfirmationNotifierResponse) walletRespSealed() {}

// BoardingUtxoConfirmedEvent is sent to registered notifiers when a new
// boarding UTXO is detected and confirmed. This event embeds the full
// BoardingIntent which contains all the information needed for the round actor
// to process the boarding.
type BoardingUtxoConfirmedEvent struct {
	actor.BaseMessage

	// BoardingIntent contains the confirmed boarding intent with address,
	// outpoint, chain info (amount, conf height/hash), and status. Embedded
	// for direct field access.
	*BoardingIntent
}

// MessageType returns the message type identifier for logging and debugging.
func (m BoardingUtxoConfirmedEvent) MessageType() string {
	return "BoardingUtxoConfirmedEvent"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m BoardingUtxoConfirmedEvent) walletMsgSealed() {}

// BlockEpochNotification wraps a chainsource.BlockEpoch to make it compatible
// with the WalletMsg sealed interface. This allows the wallet actor to receive
// block notifications directly via the actor message system instead of using an
// iterator and goroutine.
type BlockEpochNotification struct {
	actor.BaseMessage
	chainsource.BlockEpoch
}

// MessageType returns the message type identifier for logging and debugging.
func (m BlockEpochNotification) MessageType() string {
	return "BlockEpochNotification"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m BlockEpochNotification) walletMsgSealed() {}

// RefreshVTXOsRequest triggers refresh of specified VTXOs or all VTXOs
// approaching expiry. This is the primary wallet-level API for refresh.
type RefreshVTXOsRequest struct {
	actor.BaseMessage

	// TargetOutpoints specifies which VTXOs to refresh. If empty, refreshes
	// all VTXOs within the expiry threshold.
	TargetOutpoints []wire.OutPoint

	// ForceRefresh ignores the expiry threshold and refreshes immediately.
	// Used by tests or when user explicitly requests refresh.
	ForceRefresh bool
}

// MessageType returns the message type identifier for logging and debugging.
func (m *RefreshVTXOsRequest) MessageType() string {
	return "RefreshVTXOsRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *RefreshVTXOsRequest) walletMsgSealed() {}

// RefreshVTXOsResponse indicates the result of the refresh request.
type RefreshVTXOsResponse struct {
	actor.BaseMessage

	// RefreshingCount is the number of VTXOs that were queued for refresh.
	RefreshingCount int

	// Errors contains any VTXOs that couldn't be refreshed and why.
	Errors map[wire.OutPoint]error
}

// MessageType returns the message type identifier for logging and debugging.
func (m *RefreshVTXOsResponse) MessageType() string {
	return "RefreshVTXOsResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *RefreshVTXOsResponse) walletRespSealed() {}

// SelectAndLockVTXOsRequest asks the wallet actor to select VTXOs covering a
// target amount and atomically lock them to prevent double-spends. The locked
// VTXOs are returned as lightweight descriptors that the caller can use to
// build transfer inputs for OOR sends or in-round directed transfers. If the
// operation fails, the caller should send an UnlockVTXOsRequest to release
// the locks.
type SelectAndLockVTXOsRequest struct {
	actor.BaseMessage

	// TargetAmount is the minimum total value the selected VTXOs must
	// cover.
	TargetAmount btcutil.Amount
}

// MessageType returns the message type identifier for logging and debugging.
func (m *SelectAndLockVTXOsRequest) MessageType() string {
	return "SelectAndLockVTXOsRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *SelectAndLockVTXOsRequest) walletMsgSealed() {}

// SelectedVTXO describes a VTXO that was selected and locked for use as a
// transfer input (OOR or in-round). This avoids a direct dependency on the
// vtxo package in the wallet message surface (which would create an import
// cycle via vtxo → round → wallet).
type SelectedVTXO struct {
	// Outpoint is the selected VTXO's outpoint.
	Outpoint wire.OutPoint

	// Amount is the value of this VTXO in satoshis.
	Amount btcutil.Amount

	// PkScript is the output script for this VTXO.
	PkScript []byte
}

// SelectAndLockVTXOsResponse returns the VTXOs that were selected and locked.
type SelectAndLockVTXOsResponse struct {
	actor.BaseMessage

	// SelectedVTXOs is the set of VTXOs that were locked for this
	// operation. The caller should use these outpoints to look up
	// full descriptors from the VTXO store if needed.
	SelectedVTXOs []SelectedVTXO

	// TotalSelected is the sum of all selected VTXO amounts.
	TotalSelected btcutil.Amount
}

// MessageType returns the message type identifier for logging and debugging.
func (m *SelectAndLockVTXOsResponse) MessageType() string {
	return "SelectAndLockVTXOsResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *SelectAndLockVTXOsResponse) walletRespSealed() {}

// UnlockVTXOsRequest releases locks on VTXOs that were previously selected
// via SelectAndLockVTXOsRequest. This is used when a transfer or round
// participation fails or is cancelled, allowing the VTXOs to be reused.
type UnlockVTXOsRequest struct {
	actor.BaseMessage

	// Outpoints identifies the VTXOs to unlock.
	Outpoints []wire.OutPoint
}

// MessageType returns the message type identifier for logging and debugging.
func (m *UnlockVTXOsRequest) MessageType() string {
	return "UnlockVTXOsRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *UnlockVTXOsRequest) walletMsgSealed() {}

// UnlockVTXOsResponse confirms that the specified VTXOs were unlocked.
type UnlockVTXOsResponse struct {
	actor.BaseMessage

	// UnlockedCount is the number of VTXOs that were successfully
	// unlocked.
	UnlockedCount int
}

// MessageType returns the message type identifier for logging and debugging.
func (m *UnlockVTXOsResponse) MessageType() string {
	return "UnlockVTXOsResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *UnlockVTXOsResponse) walletRespSealed() {}

// SendVTXOsRequest asks the wallet actor to perform an in-round directed send.
// The wallet selects and locks VTXOs covering TotalAmount, then builds and
// forwards a TriggerVTXOSendMsg to the round actor with the selected forfeit
// inputs, recipient outputs, and change.
type SendVTXOsRequest struct {
	actor.BaseMessage

	// Recipients describes the destination outputs. Each entry contains
	// a fully constructed VTXO pkScript and amount.
	Recipients []actormsg.SendRecipient

	// TotalAmount is the sum of all recipient amounts. The wallet uses
	// this as the coin selection target.
	TotalAmount btcutil.Amount
}

// MessageType returns the message type identifier for logging and debugging.
func (m *SendVTXOsRequest) MessageType() string {
	return "SendVTXOsRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *SendVTXOsRequest) walletMsgSealed() {}

// SendVTXOsResponse indicates the result of the send request.
type SendVTXOsResponse struct {
	actor.BaseMessage

	// SelectedCount is the number of VTXOs selected as forfeit inputs.
	SelectedCount int

	// TotalSelected is the sum of all selected VTXO amounts.
	TotalSelected btcutil.Amount

	// ChangeAmount is the change returned to the sender (0 = no change).
	ChangeAmount btcutil.Amount
}

// MessageType returns the message type identifier for logging and debugging.
func (m *SendVTXOsResponse) MessageType() string {
	return "SendVTXOsResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *SendVTXOsResponse) walletRespSealed() {}

// LeaveVTXOsRequest triggers leave (offboard) of specified VTXOs. The VTXOs
// will be forfeited and their value sent to the specified destination output.
type LeaveVTXOsRequest struct {
	actor.BaseMessage

	// TargetOutpoints specifies which VTXOs to leave (offboard).
	TargetOutpoints []wire.OutPoint

	// DestOutput is the on-chain destination output where the funds will
	// be sent. This output will be included in the batch transaction.
	DestOutput *wire.TxOut
}

// MessageType returns the message type identifier for logging and debugging.
func (m *LeaveVTXOsRequest) MessageType() string {
	return "LeaveVTXOsRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *LeaveVTXOsRequest) walletMsgSealed() {}

// LeaveVTXOsResponse indicates the result of the leave request.
type LeaveVTXOsResponse struct {
	actor.BaseMessage

	// LeavingCount is the number of VTXOs that were queued for leave.
	LeavingCount int

	// Errors contains any VTXOs that couldn't be left and why.
	Errors map[wire.OutPoint]error
}

// MessageType returns the message type identifier for logging and debugging.
func (m *LeaveVTXOsResponse) MessageType() string {
	return "LeaveVTXOsResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *LeaveVTXOsResponse) walletRespSealed() {}
