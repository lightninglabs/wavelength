package indexer

import (
	"errors"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/rounds"
)

// ErrNotFound is returned when a queried record does not exist.
var ErrNotFound = errors.New("not found")

// ErrUniqueViolation is returned when a unique constraint is violated
// during an insert.
var ErrUniqueViolation = errors.New("unique constraint violation")

// VTXORow is the indexer's view of a VTXO database row.
//
// Only the fields the indexer actually reads are included; this keeps
// the domain type decoupled from the generated sqlc model.
type VTXORow struct {
	// Outpoint is the VTXO's on-chain outpoint.
	Outpoint wire.OutPoint

	// BatchOutputIndex is the tree batch output index. A nil value
	// indicates the VTXO is not linked to a VTXO tree (e.g. a
	// virtual/OOR VTXO).
	BatchOutputIndex *int32

	// Amount is the value in satoshis.
	Amount int64

	// PkScript is the output script.
	PkScript []byte

	// Status is the VTXO lifecycle status string (e.g. "live",
	// "pending", "forfeited").
	Status string

	// RoundID is the round this VTXO belongs to. A nil value
	// indicates no direct round linkage.
	RoundID *rounds.RoundID
}

// RoundRow is the indexer's view of a round database row.
//
// This type contains only the fields needed by the indexer's RPC
// response builders. The heavier fields (FinalTx, SweepKey) used
// for tree deserialization are handled internally by the TreeLoader
// implementation.
type RoundRow struct {
	// RoundID is the round identifier.
	RoundID rounds.RoundID

	// CommitmentTxid is the commitment transaction hash.
	CommitmentTxid chainhash.Hash

	// ConfirmationHeight is the chain height at which the commitment
	// transaction confirmed.
	ConfirmationHeight *int32

	// CsvDelay is the relative CSV timelock delay in blocks.
	CsvDelay int32
}

// ReceiveScript is the indexer's view of a receive script registration.
type ReceiveScript struct {
	// PrincipalMailboxID is the mailbox identity that registered this
	// script.
	PrincipalMailboxID string

	// PkScript is the registered output script.
	PkScript []byte

	// ExpiresAt is the registration expiry time.
	ExpiresAt time.Time

	// Label is a human-readable label for the registration.
	Label string

	// OwnerPubKey is the optional compressed owner pubkey for standardized
	// VTXO receive scripts. Nil means the registration does not carry Ark
	// VTXO descriptor metadata.
	OwnerPubKey []byte

	// OperatorPubKey is the optional compressed operator pubkey
	// committed to a standardized VTXO receive script. Nil means the
	// registration does not carry Ark VTXO descriptor metadata.
	OperatorPubKey []byte

	// ExitDelay is the optional CSV delay committed to a standardized VTXO
	// receive script. Zero means no Ark VTXO descriptor metadata is stored.
	ExitDelay uint32
}

// OORRecipientEventWithSession is an OOR recipient event joined with
// the originating session's identifier.
type OORRecipientEventWithSession struct {
	// RecipientPkScript is the recipient output script.
	RecipientPkScript []byte

	// EventID is the per-script monotonic event identifier.
	EventID int64

	// SessionID is the raw OOR session identifier.
	SessionID []byte

	// OutputIndex is the output index within the OOR session.
	OutputIndex int32

	// Value is the transferred amount in satoshis.
	Value int64

	// ArkPsbt is the serialized Ark PSBT for this session. The
	// client needs this to materialize the received VTXO.
	ArkPsbt []byte
}

// OORSessionCheckpoint is a single checkpoint PSBT from an OOR
// session, ordered by checkpoint index.
type OORSessionCheckpoint struct {
	// CheckpointIndex is the checkpoint's ordinal position.
	CheckpointIndex int32

	// CheckpointPsbt is the serialized checkpoint PSBT bytes.
	CheckpointPsbt []byte
}

// OORRecipientEvent is the indexer's view of an OOR recipient event
// row.
type OORRecipientEvent struct {
	// EventID is the per-script monotonic event identifier.
	EventID int64

	// RecipientPkScript is the recipient output script.
	RecipientPkScript []byte

	// OutputIndex is the output index within the OOR session.
	OutputIndex int32

	// Value is the transferred amount in satoshis.
	Value int64
}

// OORSession is the indexer's view of an OOR session row.
type OORSession struct {
	// ID is the database-assigned row identifier.
	ID int64

	// ArkPsbt is the serialized Ark PSBT for this session.
	ArkPsbt []byte
}

// OORCheckpoint is the indexer's view of an OOR checkpoint row.
type OORCheckpoint struct {
	// Psbt is the parsed checkpoint PSBT.
	Psbt *psbt.Packet
}

// VTXOEvent is the indexer's view of a VTXO lifecycle event row.
type VTXOEvent struct {
	// Outpoint is the VTXO's on-chain outpoint.
	Outpoint wire.OutPoint

	// EventID is the per-script monotonic event identifier.
	EventID int64

	// EventType is the lifecycle event type string (e.g. "created",
	// "status_changed", "terminated").
	EventType string

	// Status is the VTXO status at the time of the event.
	Status string

	// CreatedAt is the event timestamp.
	CreatedAt time.Time
}
