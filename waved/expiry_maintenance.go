package waved

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/vtxo"
)

// expiryMaintenance retains one finite primary-key sweep across warm windows.
// The host controller's mode lease serializes all access to this cursor.
// A process restart already performs the ordinary full startup reconciliation.
type expiryMaintenance struct {
	after, through *wire.OutPoint
	tip            func(context.Context) (int32, chainhash.Hash, error)
	page           func(context.Context, *wire.OutPoint, *wire.OutPoint,
		int32) (db.VTXOMaintenancePage, error)
	drive func(context.Context, []wire.OutPoint,
		vtxo.BlockEpochEvent) (vtxo.ExpiryMaintenanceResult, error)
}

// newExpiryMaintenance binds the already-started backend and existing owners.
func newExpiryMaintenance(s *Server) *expiryMaintenance {
	store := db.
		NewStore(
			s.db.DB,
			s.db.Queries,
			s.db.Backend(),
			s.subLogger(db.Subsystem),
		).
		NewVTXOStore(s.clk)

	return &expiryMaintenance{
		tip:  s.chainBackend.BestBlock,
		page: store.ReadMaintenancePage,
		drive: func(ctx context.Context, points []wire.OutPoint,
			epoch vtxo.BlockEpochEvent) (
			vtxo.ExpiryMaintenanceResult, error) {

			manager, err := s.vtxoMgrRef.UnwrapOrErr(
				fmt.Errorf("VTXO manager unavailable"),
			)
			if err != nil {
				return vtxo.ExpiryMaintenanceResult{}, err
			}

			return vtxo.MaintainExpiry(ctx, manager, points, epoch)
		},
	}
}

// run advances at most one page. Failure defers expiry work but never prevents
// mailbox pumping; no page or tip failure is a claim of completed maintenance.
func (m *expiryMaintenance) run(ctx context.Context) (uint32, bool, bool) {
	height, hash, err := m.tip(ctx)
	if err != nil {
		return 0, true, true
	}
	page, err := m.page(ctx, m.after, m.through, db.MaxVTXOMaintenancePage)
	if err != nil {
		return 0, true, true
	}
	m.through = page.Through
	progress, err := m.drive(
		ctx, page.Outpoints, vtxo.BlockEpochEvent{
			Height: height,
			Hash:   hash,
		},
	)
	partial := progress.Next < len(page.Outpoints)
	if progress.Next > 0 {
		last := page.Outpoints[progress.Next-1]
		m.after = &last
	}
	if !partial {
		m.after = page.Next
		if !page.More {
			m.after, m.through = nil, nil
		}
	}

	return uint32(progress.Checked), err != nil ||
		progress.Deferred, partial || page.More
}
