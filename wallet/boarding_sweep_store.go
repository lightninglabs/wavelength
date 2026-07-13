package wallet

import (
	"context"
	"database/sql"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// BoardingSweepStatus values are the persisted aggregate-sweep lifecycle
// states. They are stored as strings in the boarding_sweeps SQL table.
const (
	// BoardingSweepStatusPending means the sweep transaction is persisted
	// but has not yet been accepted for broadcast.
	BoardingSweepStatusPending = "pending"

	// BoardingSweepStatusPublished means the sweep transaction was
	// accepted for broadcast and is waiting for confirmed input spends.
	BoardingSweepStatusPublished = "published"

	// BoardingSweepStatusConfirmed means every tracked input has been spent
	// by the sweep transaction past the configured reorg-safety depth.
	BoardingSweepStatusConfirmed = "confirmed"

	// BoardingSweepStatusExternalResolved means every tracked input has
	// been confirmed spent, but none by the sweep we published.
	BoardingSweepStatusExternalResolved = "external_resolved"

	// BoardingSweepStatusFailed means the sweep hit a terminal local
	// error before it could be watched to completion.
	BoardingSweepStatusFailed = "failed"

	// BoardingSweepInputStatusPending means the persisted input is not
	// yet known to be broadcast.
	BoardingSweepInputStatusPending = "pending"

	// BoardingSweepInputStatusPublished means the input belongs to a
	// broadcast sweep and is waiting for a confirmed spend.
	BoardingSweepInputStatusPublished = "published"

	// BoardingSweepInputStatusSpent means the input was spent by the sweep
	// transaction past the configured reorg-safety depth.
	BoardingSweepInputStatusSpent = "spent"

	// BoardingSweepInputStatusExternalSpent means the input was confirmed
	// spent by a transaction other than the sweep we published.
	BoardingSweepInputStatusExternalSpent = "external_spent"

	// BoardingSweepInputStatusFailed means the input belonged to a sweep
	// that failed before confirmation watching completed.
	BoardingSweepInputStatusFailed = "failed"
)

// NewBoardingSweepInput describes one boarding outpoint assigned to a new
// aggregate sweep transaction.
type NewBoardingSweepInput struct {
	// Outpoint is the boarding UTXO being swept.
	Outpoint wire.OutPoint

	// Amount is the boarding UTXO value.
	Amount btcutil.Amount

	// PreviousStatus is the boarding status before the sweep moved this
	// intent into sweep_pending.
	PreviousStatus BoardingStatus
}

// NewBoardingSweep describes one aggregate boarding sweep about to be
// persisted before broadcast.
type NewBoardingSweep struct {
	// Tx is the fully signed aggregate sweep transaction.
	Tx *wire.MsgTx

	// DestinationAddress is the optional human-readable sweep destination,
	// stored verbatim. Empty when the daemon allocated a fresh wallet
	// output for the sweep.
	DestinationAddress string

	// TotalAmount is the sum of all sweep input values.
	TotalAmount btcutil.Amount

	// FeeAmount is the fee paid by the sweep transaction.
	FeeAmount btcutil.Amount

	// FeeRateSatPerVByte is the fee rate used to build the sweep.
	FeeRateSatPerVByte int64

	// VBytes is the sweep transaction virtual byte size.
	VBytes int64

	// CreatedHeight is the best chain height when the sweep was created.
	CreatedHeight int32

	// Inputs are the boarding outputs consumed by the sweep transaction.
	Inputs []NewBoardingSweepInput
}

// BoardingSweepRecord is one persisted aggregate boarding sweep and its
// inputs. This is the wallet-domain projection over the db row; sql.Null*
// fields are preserved verbatim so callers can distinguish "not yet known"
// from "zero".
type BoardingSweepRecord struct {
	// Txid is the sweep transaction id.
	Txid chainhash.Hash

	// Tx is the signed sweep transaction.
	Tx *wire.MsgTx

	// DestinationAddress is the optional address supplied at publication
	// time. Empty when the daemon generated a fresh wallet output.
	DestinationAddress string

	// TotalAmount is the sum of every sweep input value.
	TotalAmount btcutil.Amount

	// FeeAmount is the absolute fee paid by the sweep transaction.
	FeeAmount btcutil.Amount

	// FeeRateSatPerVByte is the fee rate used to build the sweep.
	FeeRateSatPerVByte int64

	// VBytes is the virtual byte size of the sweep transaction.
	VBytes int64

	// Status is the persisted sweep lifecycle status.
	Status string

	// CreatedHeight is the best chain height when the sweep was created.
	CreatedHeight int32

	// ConfirmedHeight is set once every tracked input has confirmed spent.
	ConfirmedHeight sql.NullInt32

	// LastError is the last terminal local error recorded for the sweep.
	LastError sql.NullString

	// Inputs are the boarding outpoints tracked by this sweep.
	Inputs []BoardingSweepInputRecord
}

// BoardingSweepInputRecord is one persisted boarding sweep input.
type BoardingSweepInputRecord struct {
	// Txid is the aggregate sweep transaction id.
	Txid chainhash.Hash

	// Outpoint is the boarding UTXO being watched.
	Outpoint wire.OutPoint

	// Amount is the boarding UTXO value.
	Amount btcutil.Amount

	// Status is the per-input sweep lifecycle status.
	Status string

	// SpentByTxid is the confirmed spending transaction when known.
	SpentByTxid sql.NullString

	// SpentHeight is the confirmation height of the spending transaction.
	SpentHeight sql.NullInt32
}

// BoardingSweepStore is the persistence interface the wallet actor uses to
// track in-flight aggregate boarding-timeout sweeps. The concrete
// implementation lives in db/ and translates between sqlc rows and these
// wallet-domain types.
type BoardingSweepStore interface {
	// CreatePendingBoardingSweep atomically records a sweep and moves
	// its boarding intents into sweep_pending before broadcast.
	CreatePendingBoardingSweep(
		ctx context.Context, sweep NewBoardingSweep,
	) error

	// MarkBoardingSweepPublished marks a persisted sweep and all
	// unresolved inputs as published after the transaction is accepted
	// for broadcast.
	MarkBoardingSweepPublished(
		ctx context.Context, txid chainhash.Hash,
	) error

	// MarkBoardingSweepFailed restores pending boarding intents to
	// their previous status and records a terminal local sweep failure.
	MarkBoardingSweepFailed(
		ctx context.Context, txid chainhash.Hash, failure error,
	) error

	// MarkBoardingSweepInputSpent records a confirmed spend for the
	// watched outpoint and resolves the aggregate sweep when every
	// input is spent. Returns true when the sweep transitioned to a
	// terminal status as a result of this call.
	MarkBoardingSweepInputSpent(ctx context.Context, outpoint wire.OutPoint,
		spendingTxid chainhash.Hash, spendingHeight int32) (bool, error)

	// ListBoardingSweeps returns persisted aggregate sweeps. If status
	// is non-empty, only sweeps in that lifecycle status are returned.
	ListBoardingSweeps(ctx context.Context, status string, limit,
		offset int32) ([]BoardingSweepRecord, error)

	// GetBoardingSweep returns the persisted aggregate sweep with the
	// given txid (including its inputs). Returns (nil, nil) when no
	// matching sweep is recorded. Used by the ledger-emission path at
	// confirmation time, where in-memory pendingSweeps state has
	// already been cleared or was never present after restart.
	GetBoardingSweep(ctx context.Context,
		txid chainhash.Hash) (*BoardingSweepRecord, error)

	// ListPendingBoardingSweeps returns every unresolved sweep with
	// its watched inputs. Used at startup to re-arm spend watches and
	// re-arm txconfirm tracking after a daemon restart.
	ListPendingBoardingSweeps(ctx context.Context) (
		[]BoardingSweepRecord,
		error,
	)

	// FetchBoardingIntentsBySweepableStatuses returns boarding intents
	// in any of the statuses that may still represent unswept boarding
	// outputs. Statuses outside this set are not candidates for a new
	// aggregate sweep.
	FetchBoardingIntentsBySweepableStatuses(ctx context.Context,
		statuses []BoardingStatus) ([]BoardingIntent, error)

	// GetIntent retrieves a boarding intent by its outpoint. Used when
	// re-arming spend watches at startup so the watch can carry the
	// correct height-hint and pkScript.
	GetIntent(ctx context.Context,
		outpoint wire.OutPoint) (*BoardingIntent, error)
}
