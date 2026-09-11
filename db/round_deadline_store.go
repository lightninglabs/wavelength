package db

import (
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/round"
)

// ConstrainAdmissionDeadline atomically saves and reads the earliest deadline.
// Persisting a retry cannot extend its budget or reopen a closed attempt.
func (s *RoundPersistenceStore) ConstrainAdmissionDeadline(ctx context.Context,
	roundID round.RoundID, expiresAt time.Time) (round.AdmissionDeadline,
	error) {

	var result round.AdmissionDeadline
	err := s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		err := q.ConstrainRoundAdmissionDeadline(
			ctx, sqlc.ConstrainRoundAdmissionDeadlineParams{
				RoundID:   roundID.String(),
				ExpiresAt: expiresAt.UnixNano(),
			},
		)
		if err != nil {
			return fmt.Errorf("save admission deadline: %w", err)
		}

		row, err := q.GetRoundAdmissionDeadline(ctx, roundID.String())
		if err != nil {
			return fmt.Errorf("read admission deadline: %w", err)
		}

		result = round.AdmissionDeadline{
			ExpiresAt: time.Unix(0, row.ExpiresAt).UTC(),
			Closed:    row.Closed,
		}

		return nil
	})

	return result, err
}

// CloseAdmissionDeadline records terminal admission without touching funds.
// The existing FSM failure path remains the sole owner of live cleanup.
func (s *RoundPersistenceStore) CloseAdmissionDeadline(ctx context.Context,
	roundID round.RoundID) error {

	return s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		return q.CloseRoundAdmissionDeadline(ctx, roundID.String())
	})
}

// AbandonAdmissionDeadlines fences all signing sessions lost on restart.
// This never modifies the independent signature checkpoint or releases value.
func (s *RoundPersistenceStore) AbandonAdmissionDeadlines(
	ctx context.Context) error {

	return s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		return q.AbandonRoundAdmissionDeadlines(ctx)
	})
}

// Compile-time check that production persistence provides admission budgets.
var _ round.AdmissionDeadlineStore = (*RoundPersistenceStore)(nil)
