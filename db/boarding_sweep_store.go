package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightninglabs/darepo-client/wallet"
)

// Aggregate sweep / sweep-input lifecycle constants and the persisted
// record types live in the wallet package. db/ retains only the sqlc-backed
// methods that satisfy wallet.BoardingSweepStore.

// Re-exports kept for backwards-compatible call sites inside db/.
//
// New code should reference the wallet package directly; these aliases let
// existing internal helpers continue compiling without sweeping renames.
const (
	BoardingSweepStatusPending = wallet.
		BoardingSweepStatusPending
	BoardingSweepStatusPublished = wallet.
		BoardingSweepStatusPublished
	BoardingSweepStatusConfirmed = wallet.
		BoardingSweepStatusConfirmed
	BoardingSweepStatusExternalResolved = wallet.
		BoardingSweepStatusExternalResolved
	BoardingSweepStatusFailed = wallet.
		BoardingSweepStatusFailed

	BoardingSweepInputStatusPending = wallet.
		BoardingSweepInputStatusPending
	BoardingSweepInputStatusPublished = wallet.
		BoardingSweepInputStatusPublished
	BoardingSweepInputStatusSpent = wallet.
		BoardingSweepInputStatusSpent
	BoardingSweepInputStatusExternalSpent = wallet.
		BoardingSweepInputStatusExternalSpent
	BoardingSweepInputStatusFailed = wallet.
		BoardingSweepInputStatusFailed
)

// Type aliases re-export the wallet-domain types so existing db-package
// callers (notably the methods below) compile without sweeping renames.
type (
	NewBoardingSweepInput    = wallet.NewBoardingSweepInput
	NewBoardingSweep         = wallet.NewBoardingSweep
	BoardingSweepRecord      = wallet.BoardingSweepRecord
	BoardingSweepInputRecord = wallet.BoardingSweepInputRecord
)

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
				return fmt.Errorf("mark intent pending: %w",
					err)
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
			return fmt.Errorf("mark sweep inputs published: %w",
				err)
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
				return fmt.Errorf("restore intent status: %w",
					err)
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

// GetBoardingSweep returns the persisted aggregate boarding sweep with the
// given txid (including its inputs). Returns (nil, nil) when no matching
// sweep is recorded so callers can branch on absence without inspecting
// sql.ErrNoRows.
func (b *BoardingWalletStore) GetBoardingSweep(ctx context.Context,
	txid chainhash.Hash) (*BoardingSweepRecord, error) {

	var record *BoardingSweepRecord
	err := b.db.ExecTx(ctx, ReadTxOption(), func(q BoardingStore) error {
		row, err := q.GetBoardingSweep(ctx, txid[:])
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}

			return fmt.Errorf("get boarding sweep: %w", err)
		}

		decoded, err := boardingSweepRecordFromRow(ctx, q, row)
		if err != nil {
			return err
		}

		record = &decoded

		return nil
	})

	return record, err
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
func (b *BoardingWalletStore) ListPendingBoardingSweeps(ctx context.Context) (
	[]BoardingSweepRecord, error) {

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
func (b *BoardingWalletStore) MarkBoardingSweepInputSpent(ctx context.Context,
	outpoint wire.OutPoint, spendingTxid chainhash.Hash,
	spendingHeight int32) (bool, error) {

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

		rowsAffected, err := q.MarkBoardingSweepInputSpentByOutpoint(
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

		// The SQL guards on status IN ('pending', 'published'). A
		// no-op update means this input row is already in a terminal
		// state — either we already processed this exact spend, or
		// the sweep was failed by txconfirm and a buffered chainsource
		// spend event arrived after MarkBoardingSweepFailed restored
		// the row to 'failed'. In both cases the resolution cascade
		// must be skipped so we do not overwrite intent rows ('swept'
		// over a restored Confirmed/Failed/Expired) and do not flip
		// the parent sweep row from 'failed' to 'confirmed'. The
		// caller's M-3 ErrNoRows debug branch handles this benignly.
		if rowsAffected == 0 {
			return sql.ErrNoRows
		}

		count, err := q.CountUnresolvedBoardingSweepInputs(
			ctx, sweepTxid[:],
		)
		if err != nil {
			return fmt.Errorf("count unresolved sweep inputs: %w",
				err)
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
		return BoardingSweepRecord{}, fmt.Errorf("decode raw tx: %w",
			err)
	}

	inputRows, err := q.ListBoardingSweepInputs(ctx, row.Txid)
	if err != nil {
		return BoardingSweepRecord{}, fmt.Errorf("load sweep "+
			"inputs: %w", err)
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
func boardingSweepInputRecordFromRow(row BoardingSweepInputRow) (
	BoardingSweepInputRecord, error) {

	txid, err := hashFromBytes(row.Txid)
	if err != nil {
		return BoardingSweepInputRecord{}, fmt.Errorf("decode txid: %w",
			err)
	}

	outpointHash, err := hashFromBytes(row.OutpointHash)
	if err != nil {
		return BoardingSweepInputRecord{}, fmt.Errorf("decode "+
			"outpoint hash: %w", err)
	}

	spentBy := sql.NullString{}
	if len(row.SpentByTxid) == chainhash.HashSize {
		spentHash, err := hashFromBytes(row.SpentByTxid)
		if err != nil {
			return BoardingSweepInputRecord{}, fmt.Errorf("decode "+
				"spending txid: %w", err)
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
		return chainhash.Hash{}, fmt.Errorf("expected %d-byte "+
			"hash, got %d", chainhash.HashSize, len(raw))
	}

	var hash chainhash.Hash
	copy(hash[:], raw)

	return hash, nil
}
