package txconfirm

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
)

// Msg is the sealed message surface accepted by the tx confirmation actor.
type Msg interface {
	actor.Message

	txConfirmMsgSealed()
}

// Resp is the sealed response surface returned by the tx confirmation actor.
type Resp interface {
	actor.Message

	txConfirmRespSealed()
}

// Notification is the sealed notification surface emitted to subscribers of a
// tracked transaction.
type Notification interface {
	actor.Message

	txConfirmNotificationSealed()
}

// TxState identifies the lifecycle state of one tracked transaction.
type TxState int

const (
	// TxStateNew is the initial state before any work starts.
	TxStateNew TxState = iota

	// TxStateBroadcasting indicates the initial broadcast attempt is in
	// progress, or that it failed to reach any mempool and is being
	// re-attempted on each fee-bump interval. A tx in this state has NOT
	// yet been accepted anywhere; it is distinct from
	// TxStateAwaitingConfirmation, which is reported only once the tx (or a
	// redundant parent on another path) has actually reached a mempool. A
	// tx that cannot broadcast is never failed automatically — it retries
	// until it lands and escalates to the operator after
	// BroadcastFailureAlertThreshold consecutive failures.
	TxStateBroadcasting

	// TxStateAwaitingConfirmation indicates the transaction has reached a
	// mempool and is waiting for chain confirmation.
	TxStateAwaitingConfirmation

	// TxStateFeeBumping indicates a replacement or rebroadcast attempt is
	// in progress after the initial submission.
	TxStateFeeBumping

	// TxStateConfirmed indicates the tracked transaction reached its target
	// confirmation count.
	TxStateConfirmed

	// TxStateFailed indicates the actor encountered a terminal
	// failure while
	// trying to confirm the transaction.
	TxStateFailed
)

// String returns a stable debug label for one transaction state.
func (s TxState) String() string {
	switch s {
	case TxStateNew:
		return "new"

	case TxStateBroadcasting:
		return "broadcasting"

	case TxStateAwaitingConfirmation:
		return "awaiting_confirmation"

	case TxStateFeeBumping:
		return "fee_bumping"

	case TxStateConfirmed:
		return "confirmed"

	case TxStateFailed:
		return "failed"

	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// EnsureConfirmedReq asks the actor to ensure that a signed transaction
// reaches the requested confirmation target.
//
// Deduplication is keyed by txid. If the same txid is already being tracked,
// the actor attaches the supplied subscriber to the existing tracking state
// instead of starting a second confirmation workflow.
type EnsureConfirmedReq struct {
	actor.BaseMessage

	// Tx is the fully signed transaction to confirm.
	Tx *wire.MsgTx

	// ConfirmationPkScript is the script used for the confirmation watch.
	// When empty, the actor falls back to the first transaction output
	// script.
	ConfirmationPkScript []byte

	// Label is an optional human-readable label for broadcast logging.
	Label string

	// HeightHint is the earliest height the transaction could appear in.
	HeightHint uint32

	// TargetConfs is the required confirmation count. Zero defaults to one.
	TargetConfs uint32

	// ParentFee is the absolute miner fee, in satoshis, that Tx already
	// pays. It is used only for a funded-anchor parent so a later CPFP fee
	// bump subtracts the parent's own fee and lands the combined fee on the
	// target rate. Zero (the default) is correct for zero-fee ephemeral
	// parents and for callers that do not fee-bump.
	ParentFee btcutil.Amount

	// Subscriber receives TxConfirmed or TxFailed notifications for this
	// request.
	Subscriber actor.TellOnlyRef[Notification]
}

// MessageType returns the stable message type identifier.
func (m *EnsureConfirmedReq) MessageType() string {
	return "EnsureConfirmedReq"
}

// txConfirmMsgSealed seals EnsureConfirmedReq into the package message set.
func (m *EnsureConfirmedReq) txConfirmMsgSealed() {}

// EnsureConfirmedResp acknowledges an EnsureConfirmedReq.
type EnsureConfirmedResp struct {
	actor.BaseMessage

	// Txid is the deduplication key for the tracked transaction.
	Txid chainhash.Hash

	// State is the actor's current state for this txid after processing the
	// request.
	State TxState

	// Created is true when the request created a new tracking entry and
	// false when it attached to existing state.
	Created bool
}

// MessageType returns the stable message type identifier.
func (m *EnsureConfirmedResp) MessageType() string {
	return "EnsureConfirmedResp"
}

// txConfirmRespSealed seals EnsureConfirmedResp into the package response set.
func (m *EnsureConfirmedResp) txConfirmRespSealed() {}

// CancelInterestReq asks the actor to remove one subscriber's interest in a
// tracked transaction.
type CancelInterestReq struct {
	actor.BaseMessage

	// Txid identifies the tracked transaction.
	Txid chainhash.Hash

	// SubscriberID is the ID of the subscriber to remove. Callers typically
	// pass Subscriber.ID() from the original EnsureConfirmedReq.
	SubscriberID string
}

// MessageType returns the stable message type identifier.
func (m *CancelInterestReq) MessageType() string {
	return "CancelInterestReq"
}

// txConfirmMsgSealed seals CancelInterestReq into the package message set.
func (m *CancelInterestReq) txConfirmMsgSealed() {}

// CancelInterestResp describes the result of removing subscriber interest from
// a tracked transaction.
type CancelInterestResp struct {
	actor.BaseMessage

	// Txid identifies the tracked transaction.
	Txid chainhash.Hash

	// Removed reports whether the subscriber was present and removed.
	Removed bool

	// RemainingSubscribers is the number of subscribers still attached to
	// the txid after processing the request.
	RemainingSubscribers int

	// StoppedTracking is true when the actor dropped the tracked entry
	// because no subscribers remained and the transaction was not yet in a
	// terminal state.
	StoppedTracking bool
}

// MessageType returns the stable message type identifier.
func (m *CancelInterestResp) MessageType() string {
	return "CancelInterestResp"
}

// txConfirmRespSealed seals CancelInterestResp into the package response set.
func (m *CancelInterestResp) txConfirmRespSealed() {}

// BumpNowReq asks the actor to force an immediate CPFP fee bump of an
// already-tracked transaction at an operator-supplied fee rate, rather than
// waiting for the next interval-paced bump. It is the mechanism behind an
// operator "bump this stuck transaction now" command. The transaction must
// carry a CPFP anchor for the bump to do anything; a plain transaction has no
// handle to attach a child to.
type BumpNowReq struct {
	actor.BaseMessage

	// Txid identifies the tracked transaction to bump.
	Txid chainhash.Hash

	// TargetFeeRateSatPerVByte is the fee rate the forced CPFP package
	// should target, clamped to the broadcaster's configured maximum. Zero
	// defers to the fee estimator, matching an interval-paced bump.
	TargetFeeRateSatPerVByte int64
}

// MessageType returns the stable message type identifier.
func (m *BumpNowReq) MessageType() string {
	return "BumpNowReq"
}

// txConfirmMsgSealed seals BumpNowReq into the package message set.
func (m *BumpNowReq) txConfirmMsgSealed() {}

// BumpNowResp reports the outcome of a forced fee bump.
type BumpNowResp struct {
	actor.BaseMessage

	// Txid echoes the bumped transaction's hash.
	Txid chainhash.Hash

	// State is the tracked transaction's state after the bump attempt.
	State TxState

	// Bumped is true when a fresh CPFP child was built and submitted, and
	// false when the bump was a no-op (e.g. the txid is not tracked, is
	// already terminal, or carries no anchor to bump).
	Bumped bool

	// ChildTxid is the hash of the CPFP child submitted by this bump, set
	// only when Bumped is true.
	ChildTxid *chainhash.Hash

	// EffectiveFeeRateSatPerVByte is the fee rate the submitted package
	// actually targets, set when Bumped is true. It can differ from the
	// requested rate: an over-ceiling target is clamped down, and the
	// BIP-125 replacement floor can ratchet a flat target up.
	EffectiveFeeRateSatPerVByte int64

	// Clamped is true when the operator-supplied target rate exceeded the
	// broadcaster's configured maximum and was reduced. Callers should
	// surface this: a "successful" bump at a clamped rate may still sit
	// below the market rate the operator asked for.
	Clamped bool

	// Reason is a stable human-readable explanation when Bumped is false.
	Reason string
}

// MessageType returns the stable message type identifier.
func (m *BumpNowResp) MessageType() string {
	return "BumpNowResp"
}

// txConfirmRespSealed seals BumpNowResp into the package response set.
func (m *BumpNowResp) txConfirmRespSealed() {}

// TxConfirmed notifies a subscriber that the tracked transaction reached its
// requested confirmation target.
type TxConfirmed struct {
	actor.BaseMessage

	// Txid is the confirmed transaction hash.
	Txid chainhash.Hash

	// BlockHeight is the block height where the transaction confirmed.
	BlockHeight int32

	// NumConfs is the confirmation count reported by the backend.
	NumConfs uint32
}

// MessageType returns the stable message type identifier.
func (m *TxConfirmed) MessageType() string {
	return "TxConfirmed"
}

// txConfirmNotificationSealed seals TxConfirmed into the package notification
// set.
func (m *TxConfirmed) txConfirmNotificationSealed() {}

// TxFailed notifies a subscriber that the actor encountered a terminal
// failure while trying to confirm the tracked transaction.
type TxFailed struct {
	actor.BaseMessage

	// Txid identifies the failed transaction.
	Txid chainhash.Hash

	// Reason is a stable human-readable failure reason.
	Reason string
}

// MessageType returns the stable message type identifier.
func (m *TxFailed) MessageType() string {
	return "TxFailed"
}

// txConfirmNotificationSealed seals TxFailed into the package notification
// set.
func (m *TxFailed) txConfirmNotificationSealed() {}

// MapNotification adapts txconfirm notifications into a caller-specific actor
// message type.
func MapNotification[Out actor.Message](
	targetRef actor.TellOnlyRef[Out], mapFn func(Notification) Out,
) actor.TellOnlyRef[Notification] {

	return actor.NewMapInputRef(targetRef, mapFn)
}
