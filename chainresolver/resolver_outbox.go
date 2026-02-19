package chainresolver

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// ResolverOutMsg is a sealed interface for messages emitted via the resolver
// FSM outbox. The coordinator processes these messages by translating them
// into chainsource actor calls or persistence operations.
type ResolverOutMsg interface {
	resolverOutMsgSealed()
}

// BroadcastTxOutMsg requests that a transaction be broadcast to the Bitcoin
// network via the chainsource actor.
type BroadcastTxOutMsg struct {
	// Tx is the transaction to broadcast.
	Tx *wire.MsgTx

	// Label is a human-readable label for wallet tracking.
	Label string
}

// resolverOutMsgSealed implements the sealed ResolverOutMsg interface.
func (m *BroadcastTxOutMsg) resolverOutMsgSealed() {}

// RegisterSpendWatchOutMsg requests that a spend watch be registered with the
// chainsource actor for the given outpoint. When the outpoint is spent, a
// SpendDetectedEvent will be routed back to the resolver.
type RegisterSpendWatchOutMsg struct {
	// Outpoint is the outpoint to watch for spends.
	Outpoint wire.OutPoint

	// PkScript is the public key script of the output being watched.
	PkScript []byte

	// HeightHint is the earliest block height that could contain a
	// spending transaction.
	HeightHint uint32

	// CallerID is a unique identifier for this watch registration,
	// enabling deterministic cancellation.
	CallerID string
}

// resolverOutMsgSealed implements the sealed ResolverOutMsg interface.
func (m *RegisterSpendWatchOutMsg) resolverOutMsgSealed() {}

// RegisterConfWatchOutMsg requests that a confirmation watch be registered
// with the chainsource actor for the given transaction. When the transaction
// confirms, a ConfDetectedEvent will be routed back to the resolver.
type RegisterConfWatchOutMsg struct {
	// Txid is the transaction ID to watch for confirmation.
	Txid chainhash.Hash

	// PkScript is the public key script of the output to monitor.
	PkScript []byte

	// TargetConfs is the number of confirmations to wait for.
	TargetConfs uint32

	// HeightHint is the earliest block height that could contain the
	// transaction.
	HeightHint uint32

	// CallerID is a unique identifier for this watch registration.
	CallerID string
}

// resolverOutMsgSealed implements the sealed ResolverOutMsg interface.
func (m *RegisterConfWatchOutMsg) resolverOutMsgSealed() {}

// ResolverStatusUpdateOutMsg requests that the resolver's state be persisted
// to the database for crash recovery.
type ResolverStatusUpdateOutMsg struct {
	// Outpoint identifies the VTXO being resolved.
	Outpoint wire.OutPoint

	// StateName is a string enum identifying the current resolver state.
	StateName string

	// StateDetails is an opaque JSON blob with state-specific fields.
	StateDetails []byte
}

// resolverOutMsgSealed implements the sealed ResolverOutMsg interface.
func (m *ResolverStatusUpdateOutMsg) resolverOutMsgSealed() {}

// ResolverCompletedOutMsg notifies the coordinator that this resolver has
// reached a terminal state (resolved or failed) and can be removed from
// the active resolver map.
type ResolverCompletedOutMsg struct {
	// Outpoint identifies the completed resolver's VTXO.
	Outpoint wire.OutPoint

	// FinalOutpoint is the on-chain outpoint where the VTXO value ended
	// up. Zero-valued for failed resolutions.
	FinalOutpoint wire.OutPoint

	// Success indicates whether the resolution completed successfully.
	Success bool

	// Reason provides context about the resolution outcome.
	Reason string
}

// resolverOutMsgSealed implements the sealed ResolverOutMsg interface.
func (m *ResolverCompletedOutMsg) resolverOutMsgSealed() {}
