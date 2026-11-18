package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
)

// OORStatusSummary contains the bounded status projection of one session.
// Package metadata is authoritative. Registry snapshots are included only when
// needed to recover outgoing diagnostics missing from package bindings.
type OORStatusSummary struct {
	// Metadata contains the authoritative scalar status selected in SQL.
	Metadata sqlc.OorStatus
	// ConsumedOutpoints identifies locally persisted spent inputs.
	ConsumedOutpoints []wire.OutPoint
	// CreatedOutpoints identifies locally persisted received outputs.
	CreatedOutpoints []wire.OutPoint
	// Registry supplies fallback diagnostics when bindings lack inputs.
	Registry *OORSessionRegistryRecord
}

// OORStatusCursor identifies the last entry of a newest-first status page.
// It carries the timestamp so continuation does not need the row to exist.
type OORStatusCursor struct {
	// CreatedAt is the session creation time in Unix seconds.
	CreatedAt int64
	// SessionID breaks timestamp ties using stored byte order.
	SessionID chainhash.Hash
}

// OORStatusStore reads merged package and registry status in one read snapshot.
// The status List and Get methods never load Ark PSBTs or checkpoint rows.
type OORStatusStore struct {
	*TransactionExecutor[*sqlc.Queries]
}

// NewOORStatusStore constructs the read-only status projection store.
func NewOORStatusStore(store *Store) *OORStatusStore {
	return &OORStatusStore{NewTransactionExecutor(
		store.BaseDB(), func(tx *sql.Tx) *sqlc.Queries {
			return store.Queries().WithTx(tx)
		}, store.log,
	)}
}

// List returns at most limit summaries older than the creation-time cursor. A
// nil cursor starts at the newest session. Direction zero and status -1 disable
// their respective filters. Filtering and package precedence happen before
// LIMIT and payload hydration.
func (s *OORStatusStore) List(ctx context.Context, before *OORStatusCursor,
	direction OORSessionDirection, status int32, limit int64) (
	[]OORStatusSummary, error) {

	if limit <= 0 {
		return nil, fmt.Errorf("page limit must be positive")
	}
	// The initial bound sorts after all Unix-second timestamps.
	createdAt := int64(math.MaxInt64)
	id := bytes.Repeat([]byte{0xff}, chainhash.HashSize)
	if before != nil {
		createdAt, id = before.CreatedAt, before.SessionID[:]
	}
	var result []OORStatusSummary
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.ListOORStatus(ctx, sqlc.ListOORStatusParams{
			BeforeCreatedAt: createdAt, BeforeID: id,
			DirectionFilter: int32(direction),
			StatusFilter:    status, PageLimit: limit,
		})
		if err != nil {
			return err
		}
		result = make([]OORStatusSummary, 0, len(rows))
		for _, row := range rows {
			summary, err := loadOORStatusDetails(
				ctx, q, sqlc.OorStatus(row),
			)
			if err != nil {
				return err
			}
			result = append(result, *summary)
		}

		return nil
	})

	return result, err
}

// Get uses the existing session primary keys to read one status. Missing IDs
// return sql.ErrNoRows without reading other sessions or artifact payloads.
func (s *OORStatusStore) Get(ctx context.Context, id chainhash.Hash) (
	*OORStatusSummary, error) {

	var result *OORStatusSummary
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		row, err := q.GetOORStatus(ctx, id[:])
		if err != nil {
			return err
		}
		result, err = loadOORStatusDetails(ctx, q, row)

		return err
	})

	return result, err
}

// loadOORStatusDetails loads bindings and optional outgoing diagnostics for
// one selected session in the same read transaction as the metadata query.
func loadOORStatusDetails(ctx context.Context, q *sqlc.Queries,
	row sqlc.OorStatus) (*OORStatusSummary, error) {

	result := &OORStatusSummary{Metadata: row}
	if row.HasPackage != 0 {
		bindings, err := q.ListOORVTXOBindingsBySession(
			ctx, row.SessionID,
		)
		if err != nil {
			return nil, err
		}
		for _, binding := range bindings {
			hash, err := parseHash32(binding.OutpointHash)
			if err != nil {
				return nil, err
			}
			outpoint := wire.OutPoint{
				Hash:  hash,
				Index: uint32(binding.OutpointIndex),
			}
			switch binding.LinkKind {
			case oorPackageLinkKindConsumedInputCode:
				result.ConsumedOutpoints = append(
					result.ConsumedOutpoints, outpoint,
				)

			case oorPackageLinkKindCreatedOutputCode:
				result.CreatedOutpoints = append(
					result.CreatedOutpoints, outpoint,
				)
			}
		}
		if len(result.ConsumedOutpoints) != 0 {
			return result, nil
		}
	}
	// Registry-only incoming rows already carry all usable diagnostics.
	if row.HasPackage == 0 &&
		row.Direction == int32(OORSessionDirectionIncoming) {
		return result, nil
	}

	// Filter on the registry's direction, not the package's: artifacts
	// own status and direction, but an outgoing snapshot can still supply
	// missing inputs even when the two projections disagree.
	record, err := q.GetOOROutgoingStatusSnapshot(ctx, row.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	converted, err := oorSessionRecordFromRow(record)
	if err != nil {
		return nil, err
	}
	result.Registry = &converted

	return result, nil
}
