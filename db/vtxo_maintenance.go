package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
)

// MaxVTXOMaintenancePage bounds identities selected for one warm expiry pass.
const MaxVTXOMaintenancePage int32 = 64

// VTXOMaintenancePage freezes an outpoint range, not descriptor state. New
// insertions behind the cursor are checked on a later sweep. Historical rows
// are lightweight IDs; the manager resolves only still-owned active actors.
type VTXOMaintenancePage struct {
	Outpoints []wire.OutPoint
	Through   *wire.OutPoint
	Next      *wire.OutPoint
	More      bool
}

// ReadMaintenancePage selects only primary-key identities in one read
// transaction. Nil through captures a finite ceiling; nil after begins below
// every valid Bitcoin outpoint. Reuse Through until More becomes false.
func (s *VTXOPersistenceStore) ReadMaintenancePage(ctx context.Context,
	after, through *wire.OutPoint, limit int32) (VTXOMaintenancePage,
	error) {

	var page VTXOMaintenancePage
	if limit < 1 || limit > MaxVTXOMaintenancePage {
		return page, fmt.Errorf("invalid VTXO maintenance page limit")
	}
	err := s.db.ExecTx(ctx, ReadTxOption(), func(q RoundStore) error {
		// Retryable transactions must not retain a failed attempt's
		// page.
		page = VTXOMaintenancePage{Outpoints: []wire.OutPoint{}}
		ceiling := through
		if ceiling == nil {
			row, err := q.VTXOMaintenanceCeiling(ctx)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			point, err := maintenanceOutpoint(
				row.OutpointHash, row.OutpointIndex,
			)
			if err != nil {
				return err
			}
			ceiling = &point
		}
		var lowerHash chainhash.Hash
		lowerIndex := int32(-1)
		if after != nil {
			lowerHash = after.Hash
			lowerIndex = int32(after.Index)
			if bytes.Compare(lowerHash[:], ceiling.Hash[:]) > 0 ||
				(lowerHash == ceiling.Hash &&
					after.Index > ceiling.Index) {
				return fmt.Errorf("VTXO maintenance cursor " +
					"exceeds ceiling")
			}
		}
		rows, err := q.ListVTXOMaintenancePage(ctx,
			sqlc.ListVTXOMaintenancePageParams{
				AfterHash: lowerHash[:], AfterIndex: lowerIndex,
				ThroughHash: ceiling.Hash[:],
				ThroughIndex: int32(
					ceiling.Index,
				),
				LimitCount: limit + 1,
			})
		if err != nil {
			return err
		}
		end := *ceiling
		page.Through, page.Next = &end, &end
		page.More = len(rows) > int(limit)
		if page.More {
			rows = rows[:limit]
		}
		for _, row := range rows {
			point, err := maintenanceOutpoint(
				row.OutpointHash, row.OutpointIndex,
			)
			if err != nil {
				return err
			}
			page.Outpoints = append(page.Outpoints, point)
		}
		if page.More {
			last := page.Outpoints[len(page.Outpoints)-1]
			page.Next = &last
		}

		return nil
	})

	return page, err
}

// maintenanceOutpoint decodes the existing persisted outpoint representation.
func maintenanceOutpoint(raw []byte, index int32) (wire.OutPoint, error) {
	hash, err := chainhash.NewHash(raw)
	if err != nil {
		return wire.OutPoint{}, err
	}

	return wire.OutPoint{Hash: *hash, Index: uint32(index)}, nil
}
