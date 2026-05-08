package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightninglabs/darepo-client/wallet"
)

const (
	// BoardingSweepStatusPending means the sweep transaction is persisted
	// but has not yet been accepted for broadcast.
	BoardingSweepStatusPending = "pending"

	// BoardingSweepStatusPublished means the sweep transaction was accepted
	// for broadcast and is waiting for confirmed input spends.
	BoardingSweepStatusPublished = "published"

	// BoardingSweepStatusConfirmed means every tracked input has been
	// confirmed spent.
	BoardingSweepStatusConfirmed = "confirmed"

	// BoardingSweepStatusExternalResolved means every tracked input has
	// been confirmed spent, but none by the sweep transaction we published.
	BoardingSweepStatusExternalResolved = "external_resolved"

	// BoardingSweepStatusFailed means the sweep hit a terminal local error
	// before it could be watched to completion.
	BoardingSweepStatusFailed = "failed"

	// BoardingSweepInputStatusPending means the persisted input is not yet
	// known to be broadcast.
	BoardingSweepInputStatusPending = "pending"

	// BoardingSweepInputStatusPublished means the input belongs to a
	// broadcast sweep and is waiting for a confirmed spend.
	BoardingSweepInputStatusPublished = "published"

	// BoardingSweepInputStatusSpent means the input was confirmed spent by
	// the sweep transaction we published.
	BoardingSweepInputStatusSpent = "spent"

	// BoardingSweepInputStatusExternalSpent means the input was confirmed
	// spent by a transaction other than the sweep transaction we published.
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

	// PreviousStatus is the status before the sweep moved this intent
	// into the pending lifecycle.
	PreviousStatus wallet.BoardingStatus
}

// NewBoardingSweep describes one aggregate boarding sweep to persist before
// broadcast.
type NewBoardingSweep struct {
	// Tx is the fully signed aggregate sweep transaction.
	Tx *wire.MsgTx

	// DestinationAddress is the optional human-readable sweep destination.
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

// BoardingSweepRecord is one persisted aggregate boarding sweep and its inputs.
type BoardingSweepRecord struct {
	// Txid is the sweep transaction id.
	Txid chainhash.Hash

	// Tx is the signed sweep transaction.
	Tx *wire.MsgTx

	// DestinationAddress is the optional address supplied at publication
	// time. It is empty when the daemon generated a fresh wallet output.
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

// CreatePendingBoardingSweep atomically records a sweep and moves its boarding
// intents into sweep_pending before the transaction is broadcast.
func (b *BoardingWalletStore) CreatePendingBoardingSweep(ctx context.Context,
	sweep NewBoardingSweep) error {

	if sweep.Tx == nil {
		return fmt.Errorf("sweep tx must be provided")
	}
	if len(sweep.Inputs) == 0 {
		return fmt.Errorf("sweep inputs must be provided")
	}

	var raw bytes.Buffer
	if err := sweep.Tx.Serialize(&raw); err != nil {
		return fmt.Errorf("serialize sweep tx: %w", err)
	}

	txid := sweep.Tx.TxHash()
	now := b.clock.Now().Unix()
	pendingStatus := BoardingSweepInputStatusPending

	return b.db.ExecTx(ctx, WriteTxOption(), func(q BoardingStore) error {
		params := sqlc.InsertBoardingSweepParams{
			Txid:               txid[:],
			RawTx:              raw.Bytes(),
			DestinationAddress: sweep.DestinationAddress,
			TotalAmount:        int64(sweep.TotalAmount),
			FeeAmount:          int64(sweep.FeeAmount),
			FeeRateSatPerVbyte: sweep.FeeRateSatPerVByte,
			Vbytes:             sweep.VBytes,
			Status:             BoardingSweepStatusPending,
			CreatedHeight:      sweep.CreatedHeight,
			CreatedTime:        now,
			PublishedTime:      sql.NullInt64{},
			ConfirmedHeight:    sql.NullInt32{},
			LastError:          sql.NullString{},
		}
		err := q.InsertBoardingSweep(ctx, params)
		if err != nil {
			return fmt.Errorf("insert boarding sweep: %w", err)
		}

		for _, input := range sweep.Inputs {
			prevStatus, err := statusToString(input.PreviousStatus)
			if err != nil {
				return err
			}

			err = q.InsertBoardingSweepInput(
				ctx, sqlc.InsertBoardingSweepInputParams{
					Txid:         txid[:],
					OutpointHash: input.Outpoint.Hash[:],
					OutpointIndex: int32(
						input.Outpoint.Index,
					),
					Amount:         int64(input.Amount),
					PreviousStatus: prevStatus,
					Status:         pendingStatus,
					SpentByTxid:    nil,
					SpentHeight:    sql.NullInt32{},
					LastUpdateTime: now,
				},
			)
			if err != nil {
				return fmt.Errorf("insert sweep input: %w", err)
			}

			err = q.UpdateBoardingIntentStatus(
				ctx, sqlc.UpdateBoardingIntentStatusParams{
					OutpointHash: input.Outpoint.Hash[:],
					OutpointIndex: int32(
						input.Outpoint.Index,
					),
					Status:         "sweep_pending",
					LastUpdateTime: now,
				},
			)
			if err != nil {
				return fmt.Errorf(
					"mark intent pending: %w", err,
				)
			}
		}

		return nil
	})
}

// MarkBoardingSweepPublished marks a persisted sweep and all unresolved inputs
// as published after the transaction is accepted for broadcast.
func (b *BoardingWalletStore) MarkBoardingSweepPublished(ctx context.Context,
	txid chainhash.Hash) error {

	now := b.clock.Now().Unix()
	publishedStatus := BoardingSweepInputStatusPublished
	return b.db.ExecTx(ctx, WriteTxOption(), func(q BoardingStore) error {
		err := q.MarkBoardingSweepStatus(
			ctx, sqlc.MarkBoardingSweepStatusParams{
				Txid:            txid[:],
				Status:          BoardingSweepStatusPublished,
				PublishedTime:   sqlInt64(now),
				ConfirmedHeight: sql.NullInt32{},
				LastError:       sql.NullString{},
			},
		)
		if err != nil {
			return fmt.Errorf("mark sweep published: %w", err)
		}

		err = q.MarkBoardingSweepInputsStatus(
			ctx, sqlc.MarkBoardingSweepInputsStatusParams{
				Txid:           txid[:],
				Status:         publishedStatus,
				LastUpdateTime: now,
			},
		)
		if err != nil {
			return fmt.Errorf(
				"mark sweep inputs published: %w", err,
			)
		}

		return nil
	})
}

// MarkBoardingSweepFailed restores pending boarding intents to their previous
// status and records a terminal local sweep failure.
func (b *BoardingWalletStore) MarkBoardingSweepFailed(ctx context.Context,
	txid chainhash.Hash, failure error) error {

	var errText string
	if failure != nil {
		errText = failure.Error()
	}

	now := b.clock.Now().Unix()

	return b.db.ExecTx(ctx, WriteTxOption(), func(q BoardingStore) error {
		inputs, err := q.ListBoardingSweepInputs(ctx, txid[:])
		if err != nil {
			return fmt.Errorf("list sweep inputs: %w", err)
		}

		for _, input := range inputs {
			err = q.UpdateBoardingIntentStatus(
				ctx, sqlc.UpdateBoardingIntentStatusParams{
					OutpointHash:   input.OutpointHash,
					OutpointIndex:  input.OutpointIndex,
					Status:         input.PreviousStatus,
					LastUpdateTime: now,
				},
			)
			if err != nil {
				return fmt.Errorf(
					"restore intent status: %w", err,
				)
			}
		}

		err = q.MarkBoardingSweepStatus(
			ctx, sqlc.MarkBoardingSweepStatusParams{
				Txid:            txid[:],
				Status:          BoardingSweepStatusFailed,
				PublishedTime:   sql.NullInt64{},
				ConfirmedHeight: sql.NullInt32{},
				LastError:       sqlStr(errText),
			},
		)
		if err != nil {
			return fmt.Errorf("mark sweep failed: %w", err)
		}

		err = q.MarkBoardingSweepInputsStatus(
			ctx, sqlc.MarkBoardingSweepInputsStatusParams{
				Txid:           txid[:],
				Status:         BoardingSweepInputStatusFailed,
				LastUpdateTime: now,
			},
		)
		if err != nil {
			return fmt.Errorf("mark sweep inputs failed: %w", err)
		}

		return nil
	})
}

// ListBoardingSweeps returns persisted aggregate sweeps. If status is
// non-empty, only sweeps in that lifecycle status are returned.
func (b *BoardingWalletStore) ListBoardingSweeps(ctx context.Context,
	status string, limit, offset int32) ([]BoardingSweepRecord, error) {

	var records []BoardingSweepRecord
	err := b.db.ExecTx(ctx, ReadTxOption(), func(q BoardingStore) error {
		rows, err := q.ListBoardingSweeps(
			ctx, sqlc.ListBoardingSweepsParams{
				StatusFilter: status,
				PageLimit:    limit,
				PageOffset:   offset,
			},
		)
		if err != nil {
			return fmt.Errorf("list boarding sweeps: %w", err)
		}

		records = make([]BoardingSweepRecord, 0, len(rows))
		for _, row := range rows {
			record, err := boardingSweepRecordFromRow(ctx, q, row)
			if err != nil {
				return err
			}

			records = append(records, record)
		}

		return nil
	})

	return records, err
}

// ListPendingBoardingSweeps returns every unresolved boarding sweep with its
// watched inputs.
func (b *BoardingWalletStore) ListPendingBoardingSweeps(
	ctx context.Context) ([]BoardingSweepRecord, error) {

	var records []BoardingSweepRecord
	err := b.db.ExecTx(ctx, ReadTxOption(), func(q BoardingStore) error {
		rows, err := q.ListPendingBoardingSweeps(ctx)
		if err != nil {
			return fmt.Errorf("list pending sweeps: %w", err)
		}

		records = make([]BoardingSweepRecord, 0, len(rows))
		for _, row := range rows {
			record, err := boardingSweepRecordFromRow(ctx, q, row)
			if err != nil {
				return err
			}

			records = append(records, record)
		}

		return nil
	})

	return records, err
}

// MarkBoardingSweepInputSpent records a confirmed spend for the watched
// boarding outpoint and resolves the aggregate sweep once every input is spent.
func (b *BoardingWalletStore) MarkBoardingSweepInputSpent(
	ctx context.Context, outpoint wire.OutPoint,
	spendingTxid chainhash.Hash, spendingHeight int32) (bool, error) {

	now := b.clock.Now().Unix()
	var resolved bool

	err := b.db.ExecTx(ctx, WriteTxOption(), func(q BoardingStore) error {
		sweepRow, err := q.GetBoardingSweepByInput(
			ctx, sqlc.GetBoardingSweepByInputParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
			},
		)
		if err != nil {
			return fmt.Errorf("get sweep by input: %w", err)
		}

		sweepTxid, err := hashFromBytes(sweepRow.Txid)
		if err != nil {
			return fmt.Errorf("decode sweep txid: %w", err)
		}

		inputStatus := BoardingSweepInputStatusExternalSpent
		if sweepTxid == spendingTxid {
			inputStatus = BoardingSweepInputStatusSpent
		}

		err = q.MarkBoardingSweepInputSpentByOutpoint(
			ctx, sqlc.MarkBoardingSweepInputSpentByOutpointParams{
				OutpointHash:   outpoint.Hash[:],
				OutpointIndex:  int32(outpoint.Index),
				Status:         inputStatus,
				SpentByTxid:    spendingTxid[:],
				SpentHeight:    sqlInt32(spendingHeight),
				LastUpdateTime: now,
			},
		)
		if err != nil {
			return fmt.Errorf("mark sweep input spent: %w", err)
		}

		count, err := q.CountUnresolvedBoardingSweepInputs(
			ctx, sweepTxid[:],
		)
		if err != nil {
			return fmt.Errorf(
				"count unresolved sweep inputs: %w", err,
			)
		}
		if count != 0 {
			return nil
		}

		inputs, err := q.ListBoardingSweepInputs(ctx, sweepTxid[:])
		if err != nil {
			return fmt.Errorf("list sweep inputs: %w", err)
		}
		for _, input := range inputs {
			err = q.UpdateBoardingIntentStatus(
				ctx, sqlc.UpdateBoardingIntentStatusParams{
					OutpointHash:   input.OutpointHash,
					OutpointIndex:  input.OutpointIndex,
					Status:         "swept",
					LastUpdateTime: now,
				},
			)
			if err != nil {
				return fmt.Errorf("mark intent swept: %w", err)
			}
		}

		sweepStatus := BoardingSweepStatusExternalResolved
		for _, input := range inputs {
			if input.Status == BoardingSweepInputStatusSpent {
				sweepStatus = BoardingSweepStatusConfirmed
				break
			}
		}

		err = q.MarkBoardingSweepStatus(
			ctx, sqlc.MarkBoardingSweepStatusParams{
				Txid:            sweepTxid[:],
				Status:          sweepStatus,
				PublishedTime:   sql.NullInt64{},
				ConfirmedHeight: sqlInt32(spendingHeight),
				LastError:       sql.NullString{},
			},
		)
		if err != nil {
			return fmt.Errorf("mark sweep confirmed: %w", err)
		}

		resolved = true

		return nil
	})

	return resolved, err
}

// boardingSweepRecordFromRow converts one sqlc sweep row into the daemon-facing
// record and loads its input rows.
func boardingSweepRecordFromRow(ctx context.Context, q BoardingStore,
	row BoardingSweepRow) (BoardingSweepRecord, error) {

	txid, err := hashFromBytes(row.Txid)
	if err != nil {
		return BoardingSweepRecord{}, fmt.Errorf("decode txid: %w", err)
	}

	tx := wire.NewMsgTx(arktx.TxVersion)
	if err := tx.Deserialize(bytes.NewReader(row.RawTx)); err != nil {
		return BoardingSweepRecord{}, fmt.Errorf(
			"decode raw tx: %w", err,
		)
	}

	inputRows, err := q.ListBoardingSweepInputs(ctx, row.Txid)
	if err != nil {
		return BoardingSweepRecord{}, fmt.Errorf(
			"load sweep inputs: %w", err,
		)
	}

	inputs := make([]BoardingSweepInputRecord, 0, len(inputRows))
	for _, inputRow := range inputRows {
		input, err := boardingSweepInputRecordFromRow(inputRow)
		if err != nil {
			return BoardingSweepRecord{}, err
		}

		inputs = append(inputs, input)
	}

	return BoardingSweepRecord{
		Txid:               txid,
		Tx:                 tx,
		DestinationAddress: row.DestinationAddress,
		TotalAmount:        btcutil.Amount(row.TotalAmount),
		FeeAmount:          btcutil.Amount(row.FeeAmount),
		FeeRateSatPerVByte: row.FeeRateSatPerVbyte,
		VBytes:             row.Vbytes,
		Status:             row.Status,
		CreatedHeight:      row.CreatedHeight,
		ConfirmedHeight:    row.ConfirmedHeight,
		LastError:          row.LastError,
		Inputs:             inputs,
	}, nil
}

// boardingSweepInputRecordFromRow converts one sqlc sweep input row into a
// typed outpoint record.
func boardingSweepInputRecordFromRow(
	row BoardingSweepInputRow) (BoardingSweepInputRecord, error) {

	txid, err := hashFromBytes(row.Txid)
	if err != nil {
		return BoardingSweepInputRecord{}, fmt.Errorf("decode txid: %w",
			err)
	}

	outpointHash, err := hashFromBytes(row.OutpointHash)
	if err != nil {
		return BoardingSweepInputRecord{}, fmt.Errorf(
			"decode outpoint hash: %w", err)
	}

	spentBy := sql.NullString{}
	if len(row.SpentByTxid) == chainhash.HashSize {
		spentHash, err := hashFromBytes(row.SpentByTxid)
		if err != nil {
			return BoardingSweepInputRecord{}, fmt.Errorf(
				"decode spending txid: %w", err)
		}
		spentBy = sqlStr(spentHash.String())
	}

	return BoardingSweepInputRecord{
		Txid: txid,
		Outpoint: wire.OutPoint{
			Hash:  outpointHash,
			Index: uint32(row.OutpointIndex),
		},
		Amount:      btcutil.Amount(row.Amount),
		Status:      row.Status,
		SpentByTxid: spentBy,
		SpentHeight: row.SpentHeight,
	}, nil
}

// hashFromBytes converts a database hash blob into a chainhash.Hash.
func hashFromBytes(raw []byte) (chainhash.Hash, error) {
	if len(raw) != chainhash.HashSize {
		return chainhash.Hash{}, fmt.Errorf(
			"expected %d-byte hash, got %d",
			chainhash.HashSize, len(raw),
		)
	}

	var hash chainhash.Hash
	copy(hash[:], raw)

	return hash, nil
}
