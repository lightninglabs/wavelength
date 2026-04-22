package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightningnetwork/lnd/clock"
)

// FeeScheduleStoreDB persists hot-reloaded fee schedules to the
// append-only fee_schedule_history table. Every successful
// UpdateFeeSchedule admin RPC writes one row here; on startup the
// server reads the most recent row (if any) and uses it to seed
// the fee calculator before falling through to the config-file
// schedule.
//
// Persisting the hot-reloaded schedule closes the gap where an
// operator-applied runtime change survives the process but not
// the process boundary, which would silently revert to the
// config file on every restart.
type FeeScheduleStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]

	clock clock.Clock
}

// NewFeeScheduleStoreDB creates a new FeeScheduleStoreDB from a
// Store. The clock argument is injected so tests can pin the
// persisted created_at timestamp for snapshot assertions; in
// production code pass clock.NewDefaultClock().
func NewFeeScheduleStoreDB(store *Store, c clock.Clock) *FeeScheduleStoreDB {
	txExec := NewTransactionExecutor(
		store, func(tx *sql.Tx) *sqlc.Queries {
			return store.WithTx(tx)
		}, store.log,
	)

	return &FeeScheduleStoreDB{
		TransactionExecutor: txExec,
		clock:               c,
	}
}

// InsertFeeSchedule appends a new schedule row to
// fee_schedule_history. The table is append-only by design:
// every UpdateFeeSchedule call produces one row, so operators
// can audit the schedule timeline after the fact. No dedup is
// performed; applying the same schedule twice is a valid "no-op"
// from the operator's perspective and appears as two rows.
//
// The insert runs under WriteTxOption so any schema CHECK
// violation (e.g. negative AnnualRate) rolls back atomically.
func (s *FeeScheduleStoreDB) InsertFeeSchedule(ctx context.Context,
	sched *fees.Schedule) error {

	if sched == nil {
		return errors.New("insert fee schedule: nil schedule")
	}
	if err := sched.Validate(); err != nil {
		return fmt.Errorf("insert fee schedule: %w", err)
	}

	params := sqlc.InsertFeeScheduleHistoryParams{
		AnnualRate:    sched.AnnualRate,
		BaseMarginSat: sched.BaseMarginSat,
		UtilThresholdBps: int32(
			sched.UtilizationThresholdBPS,
		),
		UtilSpreadDelta0Bps: int32(
			sched.UtilizationSpreadDelta0BPS,
		),
		UtilSpreadDelta1Bps: int32(
			sched.UtilizationSpreadDelta1BPS,
		),
		MinRefreshDeltaBlocks: int32(
			sched.MinRefreshDeltaBlocks,
		),
		MinViablePolicy: sched.MinViableVTXOPolicy.String(),
		MinViablePct:    int32(sched.MinViableVTXOPct),
		CreatedAt:       s.clock.Now().Unix(),
	}

	return s.ExecTx(
		ctx, WriteTxOption(),
		func(qtx *sqlc.Queries) error {
			return qtx.InsertFeeScheduleHistory(ctx, params)
		},
	)
}

// LatestFeeSchedule returns the most recent persisted schedule,
// or (nil, false, nil) if the history table is empty. The bool
// distinguishes "no row yet" from "error reading" so the caller
// can fall back to the config-file schedule on first startup
// without conflating it with a DB failure.
//
// Parses MinViableVTXOPolicy via fees.ParseDustPolicy so a
// malformed persisted value surfaces as an error rather than
// silently degrading to the zero policy.
func (s *FeeScheduleStoreDB) LatestFeeSchedule(ctx context.Context) (
	*fees.Schedule, bool, error) {

	var (
		out   *fees.Schedule
		found bool
	)

	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			rows, err := qtx.ListFeeScheduleHistory(ctx, 1)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return nil
			}

			row := rows[0]
			policy, err := fees.ParseDustPolicy(
				row.MinViablePolicy,
			)
			if err != nil {
				return fmt.Errorf(
					"latest fee schedule: parse "+
						"dust policy %q: %w",
					row.MinViablePolicy, err,
				)
			}

			out = &fees.Schedule{
				AnnualRate:    row.AnnualRate,
				BaseMarginSat: row.BaseMarginSat,
				UtilizationThresholdBPS: uint32(
					row.UtilThresholdBps,
				),
				UtilizationSpreadDelta0BPS: uint32(
					row.UtilSpreadDelta0Bps,
				),
				UtilizationSpreadDelta1BPS: uint32(
					row.UtilSpreadDelta1Bps,
				),
				MinViableVTXOPolicy: policy,
				MinViableVTXOPct: uint32(
					row.MinViablePct,
				),
				MinRefreshDeltaBlocks: uint32(
					row.MinRefreshDeltaBlocks,
				),
			}
			found = true

			// Cross-check with the schema-level validator
			// so a persisted row that fails Validate()
			// surfaces immediately rather than confusing
			// downstream callers.
			if err := out.Validate(); err != nil {
				return fmt.Errorf(
					"persisted fee schedule fails "+
						"validation: %w", err,
				)
			}

			return nil
		},
	)
	if err != nil {
		return nil, false, err
	}

	return out, found, nil
}
