package chainresolver

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/vtxo"
)

// ChainResolverMsg is the sealed interface for all messages that can be sent
// to the chain resolver coordinator actor. The sealed interface pattern
// ensures type safety by preventing external packages from implementing the
// interface.
type ChainResolverMsg interface {
	actor.Message
	chainResolverMsgSealed()
}

// ChainResolverResp is the sealed interface for all response messages from
// the chain resolver coordinator actor.
type ChainResolverResp interface {
	chainResolverRespSealed()
}

// ResolveTrigger indicates the reason a VTXO is being sent to the chain
// resolver for on-chain resolution.
type ResolveTrigger int

const (
	// ResolveTriggerExpiry indicates the VTXO is critically close to
	// batch expiry and must be unrolled before the operator can sweep.
	ResolveTriggerExpiry ResolveTrigger = iota

	// ResolveTriggerUser indicates the user explicitly requested to put
	// a VTXO on-chain via the wallet RPC.
	ResolveTriggerUser

	// ResolveTriggerFraudReactive indicates a prior OOR sender published
	// their VTXO on-chain; the current owner must broadcast checkpoint
	// transactions to claim their VTXO.
	ResolveTriggerFraudReactive
)

// String returns a human-readable representation of the resolve trigger.
func (t ResolveTrigger) String() string {
	switch t {
	case ResolveTriggerExpiry:
		return "expiry"

	case ResolveTriggerUser:
		return "user"

	case ResolveTriggerFraudReactive:
		return "fraud_reactive"

	default:
		return "unknown"
	}
}

// ==========================================================================
// INBOUND MESSAGES (external actors → Coordinator)
// ==========================================================================

// ExpiringVTXORequest is sent from the VTXO FSM when a VTXO is critically
// close to batch expiry and needs unilateral exit handling. The coordinator
// spawns a new resolver FSM in BroadcastingTree state for this VTXO.
type ExpiringVTXORequest struct {
	actor.BaseMessage

	// VTXO is the full descriptor of the expiring VTXO.
	VTXO *vtxo.Descriptor

	// BlocksRemaining is how many blocks until batch expiry.
	BlocksRemaining int32

	// Reason explains why the VTXO is being sent to chain resolver.
	Reason string
}

// MessageType returns the message type identifier for logging and debugging.
func (m *ExpiringVTXORequest) MessageType() string {
	return "ExpiringVTXORequest"
}

// chainResolverMsgSealed implements the sealed ChainResolverMsg interface.
func (m *ExpiringVTXORequest) chainResolverMsgSealed() {}

// UserUnrollRequest is sent from the wallet RPC when a user explicitly
// requests to put a VTXO on-chain. The coordinator spawns a new resolver
// FSM in BroadcastingTree state for this VTXO.
type UserUnrollRequest struct {
	actor.BaseMessage

	// VTXO is the full descriptor of the VTXO to unroll.
	VTXO *vtxo.Descriptor
}

// MessageType returns the message type identifier for logging and debugging.
func (m *UserUnrollRequest) MessageType() string {
	return "UserUnrollRequest"
}

// chainResolverMsgSealed implements the sealed ChainResolverMsg interface.
func (m *UserUnrollRequest) chainResolverMsgSealed() {}

// FraudReactiveRequest initiates a fraud-reactive resolver that watches
// the batch outpoint for spends by the counterparty (prior OOR sender).
// When the spend is detected, the resolver broadcasts the remaining tree
// levels and checkpoint transactions to claim the VTXO.
type FraudReactiveRequest struct {
	actor.BaseMessage

	// VTXO is the full descriptor of the VTXO to protect.
	VTXO *vtxo.Descriptor
}

// MessageType returns the message type identifier for logging and
// debugging.
func (m *FraudReactiveRequest) MessageType() string {
	return "FraudReactiveRequest"
}

// chainResolverMsgSealed implements the sealed ChainResolverMsg
// interface.
func (m *FraudReactiveRequest) chainResolverMsgSealed() {}

// SpendDetectedEvent is routed from chainsource spend watches to the
// coordinator, which dispatches it to the appropriate per-VTXO resolver
// by ResolverID.
type SpendDetectedEvent struct {
	actor.BaseMessage

	// ResolverID identifies which per-VTXO resolver should handle this
	// event.
	ResolverID wire.OutPoint

	// SpendingTx is the full spending transaction.
	SpendingTx *wire.MsgTx

	// SpendingTxid is the transaction ID of the spending transaction.
	SpendingTxid chainhash.Hash

	// SpendingHeight is the block height where the spend was confirmed.
	SpendingHeight int32
}

// MessageType returns the message type identifier for logging and debugging.
func (m *SpendDetectedEvent) MessageType() string {
	return "SpendDetectedEvent"
}

// chainResolverMsgSealed implements the sealed ChainResolverMsg interface.
func (m *SpendDetectedEvent) chainResolverMsgSealed() {}

// ConfDetectedEvent is routed from chainsource confirmation watches to the
// coordinator, which dispatches it to the appropriate per-VTXO resolver
// by ResolverID.
type ConfDetectedEvent struct {
	actor.BaseMessage

	// ResolverID identifies which per-VTXO resolver should handle this
	// event.
	ResolverID wire.OutPoint

	// Txid is the confirmed transaction ID.
	Txid chainhash.Hash

	// BlockHeight is the block height at which the transaction confirmed.
	BlockHeight int32

	// Tx is the confirmed transaction.
	Tx *wire.MsgTx
}

// MessageType returns the message type identifier for logging and debugging.
func (m *ConfDetectedEvent) MessageType() string {
	return "ConfDetectedEvent"
}

// chainResolverMsgSealed implements the sealed ChainResolverMsg interface.
func (m *ConfDetectedEvent) chainResolverMsgSealed() {}

// BlockEpochEvent is routed from chainsource block subscriptions to the
// coordinator, which broadcasts it to all active per-VTXO resolvers. Used
// for CSV delay tracking in the checkpoint broadcasting state.
type BlockEpochEvent struct {
	actor.BaseMessage

	// Height is the new block height.
	Height int32

	// Hash is the new block hash.
	Hash chainhash.Hash

	// Timestamp is the block timestamp from the header.
	Timestamp int64
}

// MessageType returns the message type identifier for logging and debugging.
func (m *BlockEpochEvent) MessageType() string {
	return "BlockEpochEvent"
}

// chainResolverMsgSealed implements the sealed ChainResolverMsg interface.
func (m *BlockEpochEvent) chainResolverMsgSealed() {}

// ==========================================================================
// RESPONSES
// ==========================================================================

// ExpiringVTXOResponse is the response to ExpiringVTXORequest.
type ExpiringVTXOResponse struct{}

// chainResolverRespSealed implements the sealed ChainResolverResp interface.
func (r *ExpiringVTXOResponse) chainResolverRespSealed() {}

// UserUnrollResponse is the response to UserUnrollRequest.
type UserUnrollResponse struct{}

// chainResolverRespSealed implements the sealed ChainResolverResp interface.
func (r *UserUnrollResponse) chainResolverRespSealed() {}

// FraudReactiveResponse is the response to FraudReactiveRequest.
type FraudReactiveResponse struct{}

// chainResolverRespSealed implements the sealed ChainResolverResp
// interface.
func (r *FraudReactiveResponse) chainResolverRespSealed() {}

// EventRoutedResponse is the response to routed events (spend, conf, epoch).
type EventRoutedResponse struct{}

// chainResolverRespSealed implements the sealed ChainResolverResp interface.
func (r *EventRoutedResponse) chainResolverRespSealed() {}

// ==========================================================================
// RESOLVER FSM EVENTS (Coordinator → Per-VTXO Resolver FSM)
// ==========================================================================

// ResolverEvent is the sealed interface for all events that drive the
// per-VTXO resolver FSM state machine.
type ResolverEvent interface {
	resolverEventSealed()
}

// StartResolveEvent triggers the resolver to begin broadcasting the tree.
// This is the initial event sent when a resolver FSM is created.
type StartResolveEvent struct {
	// Trigger indicates why this resolution was initiated.
	Trigger ResolveTrigger
}

// resolverEventSealed implements the sealed ResolverEvent interface.
func (e *StartResolveEvent) resolverEventSealed() {}

// TreeLevelConfirmedEvent indicates that all transactions at a tree level
// have been confirmed on-chain.
type TreeLevelConfirmedEvent struct {
	// Level is the tree level that was confirmed.
	Level int

	// Txid is the confirmed transaction ID.
	Txid chainhash.Hash

	// BlockHeight is the block height at which the level confirmed.
	BlockHeight int32
}

// resolverEventSealed implements the sealed ResolverEvent interface.
func (e *TreeLevelConfirmedEvent) resolverEventSealed() {}

// CheckpointConfirmedEvent indicates that a checkpoint transaction has been
// confirmed on-chain. The resolver uses this to begin the CSV wait before
// broadcasting the next checkpoint.
type CheckpointConfirmedEvent struct {
	// PackageIdx is the index of the checkpoint package that confirmed.
	PackageIdx int

	// BlockHeight is the block height at which the checkpoint confirmed.
	BlockHeight int32
}

// resolverEventSealed implements the sealed ResolverEvent interface.
func (e *CheckpointConfirmedEvent) resolverEventSealed() {}

// CSVMaturedEvent indicates that the CSV delay has elapsed since the last
// checkpoint confirmation, allowing the next checkpoint to be broadcast.
type CSVMaturedEvent struct {
	// CurrentHeight is the current block height.
	CurrentHeight int32
}

// resolverEventSealed implements the sealed ResolverEvent interface.
func (e *CSVMaturedEvent) resolverEventSealed() {}

// SpendDetectedResolverEvent indicates that the batch outpoint was spent
// on-chain (fraud-reactive path). The resolver inspects the spending tx
// to determine which tree level is already on-chain.
type SpendDetectedResolverEvent struct {
	// SpendingTx is the full spending transaction.
	SpendingTx *wire.MsgTx

	// SpendingHeight is the block height of the spend.
	SpendingHeight int32
}

// resolverEventSealed implements the sealed ResolverEvent interface.
func (e *SpendDetectedResolverEvent) resolverEventSealed() {}

// ResolverFailedEvent indicates an unrecoverable error during resolution.
type ResolverFailedEvent struct {
	// Reason describes what went wrong.
	Reason string

	// Err is the underlying error.
	Err error
}

// resolverEventSealed implements the sealed ResolverEvent interface.
func (e *ResolverFailedEvent) resolverEventSealed() {}

// ==========================================================================
// RESOLVER CONTEXT (passed to resolver FSM for VTXO metadata)
// ==========================================================================

// ResolverContext holds the VTXO metadata and OOR package chain needed by
// the resolver FSM during state transitions. This is constructed once when
// the resolver is spawned and remains immutable.
type ResolverContext struct {
	// VTXO is the full descriptor of the VTXO being resolved.
	VTXO *vtxo.Descriptor

	// TreePath is the extracted virtual transaction tree path from the
	// batch output down to this VTXO's leaf.
	TreePath *tree.Tree

	// OORPackages contains the locally known OOR package chain needed
	// for checkpoint broadcasting. Nil for non-OOR VTXOs.
	OORPackages *db.OORUnrollPackages
}
