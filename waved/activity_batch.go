package waved

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/db"
)

// ReadCurrentActivityBatch reads a finite range of replay positions with their
// current canonical projections. The embedded caller must retain this Server
// until the call returns. Wallet-service readiness publishes the store; it does
// not certify that optional projectors have caught up with remote activity.
func (s *Server) ReadCurrentActivityBatch(ctx context.Context, after,
	through int64, limit int32) (db.ActivityBatch, error) {

	if err := s.WaitForWalletServicesReady(ctx); err != nil {
		return db.ActivityBatch{}, err
	}
	if s.activityStore == nil {
		return db.ActivityBatch{}, fmt.Errorf("canonical activity " +
			"replay unavailable")
	}

	return s.activityStore.ReadCurrentBatch(ctx, after, through, limit)
}

// ActivityEventCursor captures the retained log boundary after wallet services
// start. Hosts baseline new receipt presentation only after their separate
// reconciliation pass; capturing this cursor does not perform that pass.
func (s *Server) ActivityEventCursor(ctx context.Context) (int64, error) {
	if err := s.WaitForWalletServicesReady(ctx); err != nil {
		return 0, err
	}
	if s.activityStore == nil {
		return 0, fmt.Errorf("canonical activity replay unavailable")
	}

	return s.activityStore.EventHighWater(ctx)
}
