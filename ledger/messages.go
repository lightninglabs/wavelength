package ledger

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// LedgerMsg is the message constraint for client-side ledger messages.
type LedgerMsg interface {
	actor.Message
}

// LedgerResp is the response type for the ledger actor. The
// ledger actor is fire-and-forget, so responses are always nil.
type LedgerResp interface {
	actor.Message

	ledgerRespSealed()
}

// FeePaidMsg is sent when the client pays a fee. Two flavors:
//
//   - FeeTypeBoarding / FeeTypeRefresh: an Ark protocol fee paid
//     to the operator during a round. Booked as
//     fees_paid += AmountSat / vtxo_balance -= AmountSat. Keyed
//     by RoundID via the (round_id, event_type) partial unique
//     index.
//   - FeeTypeOnchainSweep: an L1 miner fee paid by a wallet-
//     internal boarding sweep. Booked as
//     onchain_fees += AmountSat / wallet_balance -= AmountSat.
//     Has no paired VTXOReceivedMsg; keyed by the sweep txid
//     carried in IdempotencyKey via the
//     idx_client_ledger_idempotent_key partial unique index.
//     RoundID is left zero and stored as NULL.
//
// Caller contract (FeeTypeBoarding / FeeTypeRefresh only):
// FeePaidMsg accumulates the fee on top of a paired
// VTXOReceivedMsg. The VTXOReceivedMsg for the same round MUST
// carry the GROSS (pre-fee) amount -- the FeePaidMsg then nets
// vtxo_balance down to the delivered post-fee value. Sending a
// net VTXOReceivedMsg together with a FeePaidMsg will
// under-count vtxo_balance by the fee. OOR sends and receives
// are already net-of-fee and do not need a separate FeePaidMsg.
// FeeTypeOnchainSweep is a standalone entry and never pairs
// with a VTXOReceivedMsg.
type FeePaidMsg struct {
	actor.BaseMessage

	// RoundID is the 16-byte round UUID that links this fee
	// to a specific boarding or refresh round.
	RoundID [16]byte

	// AmountSat is the fee amount in satoshis. Must be
	// positive. Callers should set the paired VTXOReceivedMsg
	// AmountSat to the gross pre-fee value for the same round
	// so the two entries combine to the correct net balance.
	AmountSat int64

	// FeeType classifies the fee. Must be one of the
	// FeeType* constants (FeeTypeBoarding, FeeTypeRefresh,
	// FeeTypeOnchainSweep); any other value is rejected.
	FeeType string

	// BlockHeight is the confirmation block height.
	BlockHeight uint32

	// IdempotencyKey is an optional natural dedup key used by
	// fee events that do not carry a RoundID — the boarding
	// sweep flow for example uses the sweep txid (32 bytes).
	// Round/refresh fees leave this empty and rely on the
	// (round_id, event_type) partial unique index instead.
	IdempotencyKey []byte
}

// MessageType returns the message type name for routing.
func (m *FeePaidMsg) MessageType() string {
	return "FeePaidMsg"
}

// VTXOReceivedMsg is sent when the client receives a VTXO from
// one of three sources (see Source docstring). The ledger actor
// records the movement from the appropriate counterparty
// account into vtxo_balance.
//
// Caller contract: for Source == SourceRoundBoarding, AmountSat
// MUST be the gross (pre-fee) VTXO amount paired with a
// FeePaidMsg for the same RoundID that debits fees_paid and
// nets vtxo_balance down. For SourceRoundTransfer and
// SourceOOR, AmountSat is the net received amount and no
// FeePaidMsg is expected.
type VTXOReceivedMsg struct {
	actor.BaseMessage

	// OutpointHash is the 32-byte transaction hash of the
	// outpoint containing the received VTXO.
	OutpointHash [32]byte

	// OutpointIndex is the output index within the
	// transaction.
	OutpointIndex uint32

	// AmountSat is the VTXO value in satoshis. See the type
	// docstring for the gross-vs-net caller contract.
	AmountSat int64

	// Source classifies how the VTXO was received
	// (e.g. "round", "oor").
	Source string

	// RoundID is the 16-byte round UUID associated with this
	// VTXO.
	RoundID [16]byte
}

// MessageType returns the message type name for routing.
func (m *VTXOReceivedMsg) MessageType() string {
	return "VTXOReceivedMsg"
}

// VTXOSentMsg is sent when the client sends a VTXO to another
// participant, either out-of-round (SessionID) or inside a round
// (RoundID). Exactly one of the two identifiers must be
// non-zero; handleVTXOSent rejects messages that carry both or
// neither.
type VTXOSentMsg struct {
	actor.BaseMessage

	// SessionID is the 32-byte OOR session identifier. Zero
	// when this is an in-round send.
	SessionID [32]byte

	// RoundID is the 16-byte round UUID. Zero when this is an
	// out-of-round send.
	RoundID [16]byte

	// Outpoint identifies the specific VTXO being sent.
	// Optional -- the round-scoped idempotency index treats two
	// sends in the same round without an outpoint as duplicates,
	// so in-round refresh emissions and directed-send forfeits
	// must set this so handleVTXOSent can stamp an outpoint-
	// derived IdempotencyKey on the ledger entry. OOR single-
	// destination sends can leave the outpoint zero-valued and
	// fall back on the session_id partial index.
	Outpoint wire.OutPoint

	// AmountSat is the total value sent in satoshis.
	AmountSat int64
}

// MessageType returns the message type name for routing.
func (m *VTXOSentMsg) MessageType() string {
	return "VTXOSentMsg"
}

// ExitCostMsg is sent when the client pays an on-chain exit
// cost (e.g. unilateral exit). The ledger actor records the
// on-chain fee expense.
type ExitCostMsg struct {
	actor.BaseMessage

	// OutpointHash is the 32-byte transaction hash of the
	// exited outpoint.
	OutpointHash [32]byte

	// OutpointIndex is the output index within the
	// transaction.
	OutpointIndex uint32

	// AmountSat is the VTXO value that was exited.
	AmountSat int64

	// ExitCostSat is the on-chain fee cost of the exit.
	ExitCostSat int64

	// BlockHeight is the block height at which the exit was
	// confirmed.
	BlockHeight uint32
}

// MessageType returns the message type name for routing.
func (m *ExitCostMsg) MessageType() string {
	return "ExitCostMsg"
}

// UTXOCreatedMsg is sent when a new wallet UTXO is confirmed
// on-chain. The ledger actor writes an audit log entry
// classified by the UTXO's origin (deposit, change, etc.).
type UTXOCreatedMsg struct {
	actor.BaseMessage

	// OutpointHash is the 32-byte transaction hash.
	OutpointHash [32]byte

	// OutpointIndex is the output index within the
	// transaction.
	OutpointIndex uint32

	// AmountSat is the UTXO value in satoshis.
	AmountSat int64

	// BlockHeight is the confirmation block height.
	BlockHeight uint32

	// Classification categorizes the UTXO origin (e.g.
	// "deposit", "change", "sweep_return").
	Classification string
}

// MessageType returns the message type name for routing.
func (m *UTXOCreatedMsg) MessageType() string {
	return "UTXOCreatedMsg"
}

// UTXOSpentMsg is sent when a wallet UTXO is spent on-chain.
// The ledger actor writes an audit log entry classified by the
// spend's purpose (round_funding, sweep_return, etc.).
type UTXOSpentMsg struct {
	actor.BaseMessage

	// OutpointHash is the 32-byte transaction hash of the
	// spent outpoint.
	OutpointHash [32]byte

	// OutpointIndex is the output index within the
	// transaction.
	OutpointIndex uint32

	// AmountSat is the UTXO value in satoshis.
	AmountSat int64

	// BlockHeight is the block height at which the spend was
	// confirmed.
	BlockHeight uint32

	// Classification categorizes the spend purpose (e.g.
	// "round_funding", "unknown").
	Classification string
}

// MessageType returns the message type name for routing.
func (m *UTXOSpentMsg) MessageType() string {
	return "UTXOSpentMsg"
}

// Compile-time interface checks.
var (
	_ LedgerMsg = (*FeePaidMsg)(nil)
	_ LedgerMsg = (*VTXOReceivedMsg)(nil)
	_ LedgerMsg = (*VTXOSentMsg)(nil)
	_ LedgerMsg = (*ExitCostMsg)(nil)
	_ LedgerMsg = (*UTXOCreatedMsg)(nil)
	_ LedgerMsg = (*UTXOSpentMsg)(nil)
)
