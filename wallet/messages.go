package wallet

import (
	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
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
	Address btcaddr.Address

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

// GetBoardingBalanceResponse contains the total boarding balance, UTXO count,
// and lifecycle accounting projections used by the daemon monitoring surface.
type GetBoardingBalanceResponse struct {
	actor.BaseMessage

	// TotalBalance is the sum of all matching boarding UTXOs in
	// confirmed status (i.e. eligible to be folded into a round).
	TotalBalance btcutil.Amount

	// UtxoCount is the number of UTXOs included in the balance.
	UtxoCount int

	// UnconfirmedBalance is the sum of all zero-conf UTXOs paying to
	// tracked boarding addresses. These are payments detected on-chain but
	// not yet eligible to be folded into a round.
	UnconfirmedBalance btcutil.Amount

	// UnconfirmedUtxoCount is the number of zero-conf boarding UTXOs
	// included in UnconfirmedBalance.
	UnconfirmedUtxoCount int

	// AdoptedBalance is the total amount of boarding UTXOs accepted into a
	// persisted round checkpoint whose resulting VTXOs are not yet live.
	AdoptedBalance btcutil.Amount

	// PendingSweepBalance is the total amount of boarding UTXOs that
	// have been included in a published-but-unconfirmed boarding-sweep
	// transaction (status "sweep_pending"). These funds are no longer
	// reported under TotalBalance and have not yet returned to the
	// on-chain wallet, so the field surfaces value currently in flight
	// to L1.
	PendingSweepBalance btcutil.Amount

	// SweptBalance is the cumulative total of boarding UTXOs recovered
	// via the timeout-path sweep flow (status "swept"). Historical
	// accounting only; once swept the funds reappear under the
	// on-chain wallet's confirmed balance.
	SweptBalance btcutil.Amount
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

// GetConfirmedBoardingIntentsRequest asks the wallet actor for the currently
// confirmed boarding intents. Round retry after restart uses this to rebuild
// the boarding side of round assembly from the wallet's persisted source of
// truth.
type GetConfirmedBoardingIntentsRequest struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier for logging and debugging.
func (m *GetConfirmedBoardingIntentsRequest) MessageType() string {
	return "GetConfirmedBoardingIntentsRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *GetConfirmedBoardingIntentsRequest) walletMsgSealed() {}

// GetConfirmedBoardingIntentsResponse returns the confirmed boarding intents
// currently tracked by the wallet actor.
type GetConfirmedBoardingIntentsResponse struct {
	actor.BaseMessage

	// Intents are the confirmed boarding intents ready for round
	// registration.
	Intents []BoardingIntent
}

// MessageType returns the message type identifier for logging and debugging.
func (m *GetConfirmedBoardingIntentsResponse) MessageType() string {
	return "GetConfirmedBoardingIntentsResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *GetConfirmedBoardingIntentsResponse) walletRespSealed() {}

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

// ProcessTipTickNotification fires periodically from the wallet's own
// tick loop. The handler checks whether the latest known chain tip
// (recorded by BlockEpochNotification, an atomic store with no actor
// work) has advanced past the last successfully-processed height, and
// if so runs the per-tip work (ListUnspent + boarding-sweep resume
// kick) once for the latest tip. This decouples per-block notification
// rate from actor processing rate, so a 200-block catch-up burst
// resolves to a single tick's worth of work instead of saturating the
// mailbox with one heavy handler per block.
type ProcessTipTickNotification struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier for logging and
// debugging.
func (m ProcessTipTickNotification) MessageType() string {
	return "ProcessTipTickNotification"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m ProcessTipTickNotification) walletMsgSealed() {}

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

// CustomRefreshInput describes one caller-supplied custom-policy VTXO input
// that should be forfeited into a new round output. The wallet does not select
// it from live balance, but it does materialize a PendingForfeit signer actor
// so the later connector-bound round request can collect the local signature.
type CustomRefreshInput struct {
	// Outpoint identifies the custom VTXO to refresh.
	Outpoint wire.OutPoint

	// Amount is the VTXO value in satoshis.
	Amount btcutil.Amount

	// PkScript is the script committed to by the custom VTXO.
	PkScript []byte

	// PolicyTemplate is the semantic custom VTXO policy template.
	PolicyTemplate []byte

	// ClientKey is the daemon identity key used for the local policy
	// signature.
	ClientKey keychain.KeyDescriptor

	// OperatorKey is the Ark operator key committed to by the policy.
	OperatorKey *btcec.PublicKey

	// RelativeExpiry records the custom policy's CSV delay for the
	// temporary signer descriptor.
	RelativeExpiry uint32

	// RoundID identifies the round lineage that created this custom
	// VTXO. It is resolved from the authoritative indexer before the
	// temporary signer descriptor is persisted.
	RoundID string

	// CommitmentTxID is the commitment tx anchoring this custom VTXO.
	CommitmentTxID chainhash.Hash

	// BatchExpiry is the absolute batch expiry height for the custom
	// VTXO lineage.
	BatchExpiry int32

	// ChainDepth records how many OOR checkpoint hops separate this
	// custom VTXO from its commitment tx.
	ChainDepth int

	// CreatedHeight records the block height where the commitment tx was
	// confirmed.
	CreatedHeight int32

	// Ancestry carries the commitment-tree fragments needed for any later
	// unilateral path.
	Ancestry []types.Ancestry

	// AuthSpend is the proof/auth spend path used for join authorization.
	AuthSpend *arkscript.SpendPath

	// ForfeitSpend is the operator-backed spend path used for the actual
	// round forfeit transaction after connector assignment.
	ForfeitSpend *arkscript.SpendPath
}

// CustomRefreshOutput describes the replacement VTXO requested for one custom
// refresh input.
type CustomRefreshOutput struct {
	// Amount is the requested replacement VTXO value in satoshis.
	Amount btcutil.Amount

	// PolicyTemplate is the semantic policy template for the replacement
	// VTXO.
	PolicyTemplate []byte

	// PkScript optionally pins the replacement VTXO script.
	PkScript []byte

	// FixedAmount requires the refresh round quote to preserve Amount
	// exactly. This is for contract outputs, such as vHTLCs, where
	// paying refresh fees by shrinking the replacement output is unsafe.
	FixedAmount bool
}

// RefreshCustomVTXOsRequest queues custom-policy VTXOs for refresh in the next
// round. The caller supplies every old-input and replacement-output detail, so
// the wallet does not select wallet-owned live VTXOs. It still activates
// temporary PendingForfeit signer actors for the old inputs.
type RefreshCustomVTXOsRequest struct {
	actor.BaseMessage

	// Inputs are the custom VTXOs to forfeit.
	Inputs []CustomRefreshInput

	// Outputs are the replacement VTXOs. The current API expects one
	// output per input.
	Outputs []CustomRefreshOutput
}

// MessageType returns the message type identifier for logging and debugging.
func (m *RefreshCustomVTXOsRequest) MessageType() string {
	return "RefreshCustomVTXOsRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *RefreshCustomVTXOsRequest) walletMsgSealed() {}

// RefreshCustomVTXOsResponse indicates the result of a custom refresh request.
type RefreshCustomVTXOsResponse struct {
	actor.BaseMessage

	// RefreshingCount is the number of custom VTXOs queued for refresh.
	RefreshingCount int
}

// MessageType returns the message type identifier for logging and debugging.
func (m *RefreshCustomVTXOsResponse) MessageType() string {
	return "RefreshCustomVTXOsResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *RefreshCustomVTXOsResponse) walletRespSealed() {}

// DropCustomRefreshVTXOsRequest asks the wallet to remove temporary
// PendingForfeit signer actors that were activated for a custom refresh
// package whose round registration did not start.
type DropCustomRefreshVTXOsRequest struct {
	actor.BaseMessage

	// Outpoints identifies the custom refresh inputs to drop.
	Outpoints []wire.OutPoint
}

// MessageType returns the message type identifier for logging and debugging.
func (m *DropCustomRefreshVTXOsRequest) MessageType() string {
	return "DropCustomRefreshVTXOsRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *DropCustomRefreshVTXOsRequest) walletMsgSealed() {}

// DropCustomRefreshVTXOsResponse reports how many temporary custom refresh
// signer actors were removed.
type DropCustomRefreshVTXOsResponse struct {
	actor.BaseMessage

	// DroppedCount is the number of custom refresh inputs removed.
	DroppedCount int
}

// MessageType returns the message type identifier for logging and debugging.
func (m *DropCustomRefreshVTXOsResponse) MessageType() string {
	return "DropCustomRefreshVTXOsResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *DropCustomRefreshVTXOsResponse) walletRespSealed() {}

// SelectAndLockVTXOsRequest asks the wallet actor to select VTXOs covering a
// target amount and atomically lock them to prevent double-spends. The locked
// VTXOs are returned as descriptors that the caller can use to build OOR
// transfer inputs. If the transfer fails, the caller should send an
// UnlockVTXOsRequest to release the locks.
type SelectAndLockVTXOsRequest struct {
	actor.BaseMessage

	// TargetAmount is the minimum total value the selected VTXOs must
	// cover.
	TargetAmount btcutil.Amount

	// MinChangeAmount, when positive, asks selection to avoid a
	// non-zero residual below this amount. Exact spends are still valid.
	MinChangeAmount btcutil.Amount
}

// MessageType returns the message type identifier for logging and debugging.
func (m *SelectAndLockVTXOsRequest) MessageType() string {
	return "SelectAndLockVTXOsRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *SelectAndLockVTXOsRequest) walletMsgSealed() {}

// SelectedVTXO describes a VTXO that was selected and locked for use as
// a transfer input. This avoids a direct dependency on the vtxo package
// in the wallet message surface (which would create an import cycle via
// vtxo → round → wallet).
type SelectedVTXO struct {
	// Outpoint is the selected VTXO's outpoint.
	Outpoint wire.OutPoint

	// Amount is the value of this VTXO in satoshis.
	Amount btcutil.Amount

	// PkScript is the output script for this VTXO. This also serves
	// as the owner leaf script for OOR checkpoint construction.
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
// via SelectAndLockVTXOsRequest. This is used when an OOR transfer fails
// or is cancelled, allowing the VTXOs to be used in future operations.
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

// CompleteSpendVTXOsRequest marks VTXOs as fully spent after a successful
// OOR transfer. This transitions each VTXO from SpendingState to the
// terminal SpentState via the VTXO manager.
type CompleteSpendVTXOsRequest struct {
	actor.BaseMessage

	// Outpoints identifies the VTXOs to mark as spent.
	Outpoints []wire.OutPoint
}

// MessageType returns the message type identifier for logging and debugging.
func (m *CompleteSpendVTXOsRequest) MessageType() string {
	return "CompleteSpendVTXOsRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *CompleteSpendVTXOsRequest) walletMsgSealed() {}

// CompleteSpendVTXOsResponse confirms the spend completion.
type CompleteSpendVTXOsResponse struct {
	actor.BaseMessage

	// CompletedCount is the number of VTXOs successfully marked as spent.
	CompletedCount int
}

// MessageType returns the message type identifier for logging and debugging.
func (m *CompleteSpendVTXOsResponse) MessageType() string {
	return "CompleteSpendVTXOsResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *CompleteSpendVTXOsResponse) walletRespSealed() {}

// LeaveVTXOsRequest triggers leave (offboard) of specified VTXOs. The
// VTXOs will be forfeited and their on-chain value lands at the
// destination script; the server decides the binding per-leave amount
// at seal time via the seal-time fee builder. The wallet simply
// marks the first leave output as IsChange=true so the server stamps
// the residual there.
type LeaveVTXOsRequest struct {
	actor.BaseMessage

	// TargetOutpoints specifies which VTXOs to leave (offboard).
	TargetOutpoints []wire.OutPoint

	// DestOutput carries the default destination pkScript applied to
	// every target outpoint that is not overridden in DestOutputs.
	// Under #270 its Value field is used only as a target hint — the
	// binding amount comes from the server's JoinRoundQuote at seal
	// time.
	DestOutput *wire.TxOut

	// DestOutputs optionally overrides DestOutput on a per-outpoint
	// basis. When an entry is present for an outpoint, the wallet
	// handler uses its PkScript for that leave output; any outpoint
	// without an entry falls back to DestOutput. This lets a single
	// LeaveVTXOsRequest batch offboards to distinct on-chain
	// destinations in one round. Like DestOutput, the binding amount
	// is decided by the server's JoinRoundQuote — the entry's Value
	// field is treated only as a target hint.
	DestOutputs map[wire.OutPoint]*wire.TxOut
}

// MessageType returns the message type identifier for logging and debugging.
func (m *LeaveVTXOsRequest) MessageType() string {
	return "LeaveVTXOsRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *LeaveVTXOsRequest) walletMsgSealed() {}

// BoardRequest triggers the wallet to board all confirmed boarding UTXOs
// into the next round. Under the #270 seal-time fee handshake the server
// decides the operator fee when the round seals; the wallet ships the full
// confirmed boarding balance as VTXO intent targets and the server stamps
// the residual at seal time. This is a non-blocking operation; use
// ListRounds/WatchRounds to observe round progress.
type BoardRequest struct {
	actor.BaseMessage

	// TargetVTXOCount is the requested number of boarded VTXOs. Zero means
	// one output, preserving the legacy single-VTXO board behavior.
	TargetVTXOCount uint32

	// NoPersist opts out of restart-safe Board replay. When true, the
	// wallet skips the pending-intent outbox write entirely so a crash
	// between admission and round seal silently drops the request.
	// Default false is the safe behavior: the wallet persists one
	// intent anchored to every admitted confirmed outpoint and replays
	// via its own startup self-Tell. The startup replay path always
	// sets this to false so a replay re-persists with a fresh
	// timestamp.
	NoPersist bool

	// PolicyTemplate optionally pins the arkscript policy for every boarded
	// VTXO output. When nil, the round actor synthesizes the standard
	// collaborative policy with a freshly derived owner key. When set, the
	// boarded outputs adopt this serialized template verbatim, so a client
	// can board directly into a custom-owned VTXO (e.g. one owned by an
	// external FROST aggregate key). It is persisted with the board intent
	// and re-applied on restart replay.
	PolicyTemplate []byte

	// PkScript optionally pins the taproot output script for the boarded
	// VTXOs. Only valid alongside PolicyTemplate; when empty the script is
	// derived from the template.
	PkScript []byte
}

// MessageType returns the message type identifier for logging and debugging.
func (m *BoardRequest) MessageType() string {
	return "BoardRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *BoardRequest) walletMsgSealed() {}

// BoardResponse contains the result of a boarding request. The actual round
// registration happens asynchronously in the round actor.
type BoardResponse struct {
	actor.BaseMessage

	// BoardingBalance is the total confirmed boarding balance found in the
	// wallet.
	BoardingBalance btcutil.Amount

	// VTXOAmount is the VTXO output amount that was registered for the
	// next round. When multiple VTXOs are requested, this is the total of
	// VTXOAmounts and is kept for existing internal callers.
	VTXOAmount btcutil.Amount

	// VTXOAmounts are the per-output target amounts registered for the
	// next round before seal-time operator fees are stamped.
	VTXOAmounts []btcutil.Amount
}

// MessageType returns the message type identifier for logging and debugging.
func (m *BoardResponse) MessageType() string {
	return "BoardResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *BoardResponse) walletRespSealed() {}

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

// SendRecipient describes a single recipient for an in-round directed
// send. The PkScript is the fully resolved VTXO output script.
type SendRecipient struct {
	// PkScript is the recipient's VTXO output script. For pubkey
	// destinations this is derived from the recipient's key, the
	// operator's key, and the VTXO exit delay via
	// tree.NewVTXODescriptor. For pk_script destinations the caller
	// provides the raw script directly.
	PkScript []byte

	// Amount is the value to send to this recipient in satoshis.
	Amount btcutil.Amount

	// ClientKey is the recipient's public key for the collaborative
	// spend path. Nil for pk_script destinations where the key is
	// embedded in the script but not provided separately.
	ClientKey *btcec.PublicKey
}

// SendVTXOsRequest asks the wallet to execute an in-round directed
// send. The wallet atomically selects and reserves VTXOs for
// cooperative consumption, builds the IntentPackage (forfeits +
// recipient VTXOs + change), and registers it with the round actor.
type SendVTXOsRequest struct {
	actor.BaseMessage

	// Recipients is the list of send destinations with resolved
	// pkScripts and amounts.
	Recipients []SendRecipient

	// OperatorFee is the fee deducted from the total to pay the
	// operator.
	OperatorFee btcutil.Amount

	// DustLimit is the effective minimum viable VTXO output amount.
	// Change below this threshold causes the send to be rejected.
	DustLimit btcutil.Amount

	// OperatorKey is the operator's public key for constructing
	// new VTXO descriptors (change output).
	OperatorKey *btcec.PublicKey

	// VTXOExitDelay is the CSV delay for the unilateral exit path
	// of new VTXOs.
	VTXOExitDelay uint32

	// DryRun when true validates coin selection and immediately
	// releases the reservation without registering with the round.
	DryRun bool
}

// MessageType returns the message type identifier for logging.
func (m *SendVTXOsRequest) MessageType() string {
	return "SendVTXOsRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *SendVTXOsRequest) walletMsgSealed() {}

// SendVTXOsResponse contains the result of a directed send request.
type SendVTXOsResponse struct {
	actor.BaseMessage

	// Status is "submitted" for real sends or "preview" for dry-run.
	Status string

	// SelectedCount is the number of VTXOs selected as inputs.
	SelectedCount int

	// TotalSelected is the sum of selected VTXO amounts.
	TotalSelected btcutil.Amount

	// ChangeAmount is the change returned to the sender. Zero if
	// the selection exactly covered the total.
	ChangeAmount btcutil.Amount
}

// MessageType returns the message type identifier for logging.
func (m *SendVTXOsResponse) MessageType() string {
	return "SendVTXOsResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *SendVTXOsResponse) walletRespSealed() {}

// SendOnChainRequest asks the wallet actor to plan and submit an atomic
// onchain payment from VTXOs: select the inputs, build one fixed leave
// output and one change VTXO (bounded mode) or one fee-absorbing leave
// output (sweep-all mode), and register the resulting intent with the
// round actor with eager registration.
//
// The mode is implicit in the field set: a non-empty SweepOutpoints
// means sweep-all (drain those VTXOs), otherwise it is a bounded send
// for TargetAmountSat. The RPC layer enforces that exactly one shape is
// populated before dispatching the request to the wallet.
type SendOnChainRequest struct {
	actor.BaseMessage

	// DestinationPkScript is the on-chain destination script for the
	// leave output. The script is validated and resolved by the RPC
	// layer before dispatch; the wallet treats it as opaque.
	DestinationPkScript []byte

	// TargetAmountSat is the exact on-chain amount the caller wants
	// to land at DestinationPkScript. Must be > 0 for a bounded send
	// (empty SweepOutpoints). The wallet selects VTXOs whose summed
	// value covers TargetAmountSat plus an OperatorFee + DustLimit
	// headroom so a residual change VTXO can land above dust.
	TargetAmountSat btcutil.Amount

	// SweepOutpoints carries the live-VTXO outpoint set enumerated by
	// the RPC server for sweep-all mode; a non-empty value selects
	// sweep-all. Every live VTXO is drained to DestinationPkScript
	// with no change VTXO; the single leave output absorbs the
	// residual under the #270 fee handshake. Empty means a bounded
	// send. The wallet does not enumerate live VTXOs itself; the RPC
	// layer's vtxoStore listing is the single source of truth.
	SweepOutpoints []wire.OutPoint

	// OperatorFee is the daemon's current operator-fee hint
	// (typically OperatorTerms.MinOperatorFee). Used as
	// coin-selection headroom in bounded mode so the residual change
	// VTXO does not land below dust. Advisory only under #270; the
	// binding fee comes from the server's seal-time quote.
	OperatorFee btcutil.Amount

	// DustLimit is the effective change-VTXO floor. Added to
	// OperatorFee when computing coin-selection headroom in bounded
	// mode.
	DustLimit btcutil.Amount

	// OperatorKey is the operator's pubkey for the change-VTXO
	// policy template. Required in bounded mode; unused in sweep-all.
	OperatorKey *btcec.PublicKey

	// VTXOExitDelay is the CSV delay for the change VTXO's exit
	// path. Required in bounded mode; unused in sweep-all.
	VTXOExitDelay uint32

	// DryRun validates inputs and reserves a selection without
	// submitting the round intent. The reservation is released
	// immediately after the preview is built.
	DryRun bool
}

// IsSweepAll reports whether the request drains the wallet (sweep-all)
// rather than sending a bounded TargetAmountSat. Sweep-all is implied
// by a non-empty SweepOutpoints set.
func (m *SendOnChainRequest) IsSweepAll() bool {
	return len(m.SweepOutpoints) > 0
}

// MessageType returns the message type identifier for logging.
func (m *SendOnChainRequest) MessageType() string {
	return "SendOnChainRequest"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *SendOnChainRequest) walletMsgSealed() {}

// ReplaySendOnChainIntent is an internal self-Tell carrying one persisted
// SendOnChain intent from the startup replay pass into the wallet's own
// mailbox. The handler re-reserves the intent's exact anchor outpoints,
// rebuilds the intent package from the TLV payload, and re-registers it
// with the round actor. Not part of the public RPC surface; only the
// sendOnChainIntentReplayer emits it.
type ReplaySendOnChainIntent struct {
	actor.BaseMessage

	// Intent is the persisted outbox row to re-issue.
	Intent PendingIntent
}

// MessageType returns the message type identifier for logging.
func (m *ReplaySendOnChainIntent) MessageType() string {
	return "ReplaySendOnChainIntent"
}

// walletMsgSealed implements the sealed WalletMsg interface.
func (m *ReplaySendOnChainIntent) walletMsgSealed() {}

// SendOnChainStatus enumerates the terminal outcomes of a
// SendOnChainRequest as surfaced to the caller.
type SendOnChainStatus uint8

const (
	// SendOnChainStatusSubmitted indicates the onchain send intent
	// was registered with the round actor for the next round.
	SendOnChainStatusSubmitted SendOnChainStatus = iota

	// SendOnChainStatusPreview indicates a dry-run: inputs were
	// validated and a selection previewed without submitting an
	// intent.
	SendOnChainStatusPreview
)

// String returns the wire string form of the status, consumed by the
// RPC layer when projecting onto the proto response.
func (s SendOnChainStatus) String() string {
	switch s {
	case SendOnChainStatusSubmitted:
		return "submitted"

	case SendOnChainStatusPreview:
		return "preview"

	default:
		return "unknown"
	}
}

// SendOnChainResponse carries the outcome of a SendOnChainRequest.
type SendOnChainResponse struct {
	actor.BaseMessage

	// Status is Submitted for a successfully registered intent or
	// Preview for a dry-run.
	Status SendOnChainStatus

	// IntentID is the stable pending-intent id for a submitted send —
	// a deterministic hash of the consumed outpoints and payload, so it
	// survives a restart and represents a multi-input sweep as one id.
	// It is the zero value for a dry-run preview (no intent is persisted).
	IntentID PendingIntentID

	// ActualAmountSat is the on-chain amount that will land at
	// DestinationPkScript. In bounded mode this equals the
	// TargetAmountSat the caller requested (the server stamps fee
	// deviations onto the change VTXO). In SweepAll mode this is
	// the pre-fee Σ(SelectedOutpoints); the actual landing amount
	// is reduced by the server's seal-time operator fee.
	ActualAmountSat btcutil.Amount

	// SelectedOutpoints is the set of VTXOs forfeited for this send,
	// in selection order.
	SelectedOutpoints []wire.OutPoint

	// TotalSelected is the sum of all SelectedOutpoints' VTXO
	// amounts.
	TotalSelected btcutil.Amount

	// ChangeAmount is the projected change-VTXO value in bounded
	// mode (TotalSelected − TargetAmountSat, before fee). Zero for
	// SweepAll. The on-chain value is finalized by the server at
	// seal time and may differ by the operator fee.
	ChangeAmount btcutil.Amount
}

// MessageType returns the message type identifier for logging.
func (m *SendOnChainResponse) MessageType() string {
	return "SendOnChainResponse"
}

// walletRespSealed implements the sealed WalletResp interface.
func (m *SendOnChainResponse) walletRespSealed() {}
