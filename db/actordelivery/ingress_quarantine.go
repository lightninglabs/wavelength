package actordelivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/db"
	adsqlc "github.com/lightninglabs/wavelength/db/actordelivery/sqlc"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
)

// quarantineIngress preserves the original envelope and failure once. Per-lane
// reservations prevent one peer from consuming the global count and byte
// limits; existing evidence is never overwritten.
func quarantineIngress(ctx context.Context, q ActorDeliveryQueries,
	entry mailboxconn.IngressQuarantine, now time.Time) error {

	_, err := q.GetIngressQuarantine(ctx, entry.ID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	count, err := q.InsertIngressQuarantine(
		ctx, adsqlc.InsertIngressQuarantineParams{
			ID:            entry.ID,
			Lane:          entry.Lane,
			Envelope:      entry.Envelope,
			Reason:        entry.Reason,
			CreatedAt:     now.Unix(),
			EnvelopeBytes: int64(len(entry.Envelope)),
		},
	)
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("ingress quarantine capacity reached; " +
			"envelope remains unacknowledged")
	}

	return nil
}

// listIngressQuarantine reads one connection's unresolved wire evidence.
func listIngressQuarantine(ctx context.Context, q ActorDeliveryQueries,
	lane string) ([]mailboxconn.IngressQuarantine, error) {

	rows, err := q.ListIngressQuarantine(ctx, lane)
	if err != nil {
		return nil, err
	}
	entries := make([]mailboxconn.IngressQuarantine, 0, len(rows))
	for _, row := range rows {
		entries = append(
			entries, mailboxconn.IngressQuarantine{
				ID:       row.ID,
				Lane:     row.Lane,
				Envelope: row.Envelope,
				Reason:   row.Reason,
				Attempts: row.Attempts,
			},
		)
	}

	return entries, nil
}

// QuarantineIngress retains poison evidence atomically with the caller's fold.
func (s *Store) QuarantineIngress(ctx context.Context,
	entry mailboxconn.IngressQuarantine) error {

	return s.db.ExecTx(
		ctx, db.WriteTxOption(),
		func(q ActorDeliveryQueries) error {
			return quarantineIngress(
				ctx, q, entry, s.clock.Now(),
			)
		},
	)
}

// ListIngressQuarantine returns unresolved entries for one connection.
func (s *Store) ListIngressQuarantine(ctx context.Context, lane string) (
	[]mailboxconn.IngressQuarantine, error) {

	var entries []mailboxconn.IngressQuarantine
	err := s.db.ExecTx(
		ctx, db.ReadTxOption(),
		func(q ActorDeliveryQueries) error {
			var err error
			entries, err = listIngressQuarantine(ctx, q, lane)

			return err
		},
	)

	return entries, err
}

// NoteIngressQuarantineAttempt records one explicit process-start retry.
func (s *Store) NoteIngressQuarantineAttempt(ctx context.Context,
	id string) error {

	return s.db.ExecTx(
		ctx, db.WriteTxOption(),
		func(q ActorDeliveryQueries) error {
			return q.NoteIngressQuarantineAttempt(
				ctx, id,
			)
		},
	)
}

// DeleteIngressQuarantine removes evidence after a proven durable handoff.
func (s *Store) DeleteIngressQuarantine(ctx context.Context, id string) error {
	return s.db.ExecTx(
		ctx, db.WriteTxOption(),
		func(q ActorDeliveryQueries) error {
			return q.DeleteIngressQuarantine(
				ctx, id,
			)
		},
	)
}

// QuarantineIngress joins the fold's transaction without an independent commit.
func (s *TxActorDeliveryStore) QuarantineIngress(ctx context.Context,
	entry mailboxconn.IngressQuarantine) error {

	return quarantineIngress(ctx, s.querier, entry, s.clock.Now())
}

// ListIngressQuarantine reads unresolved entries in the active transaction.
func (s *TxActorDeliveryStore) ListIngressQuarantine(ctx context.Context,
	lane string) ([]mailboxconn.IngressQuarantine, error) {

	return listIngressQuarantine(ctx, s.querier, lane)
}

// NoteIngressQuarantineAttempt records recovery alongside its outcome.
func (s *TxActorDeliveryStore) NoteIngressQuarantineAttempt(ctx context.Context,
	id string) error {

	return s.querier.NoteIngressQuarantineAttempt(ctx, id)
}

// DeleteIngressQuarantine removes evidence in the consumer handoff transaction.
func (s *TxActorDeliveryStore) DeleteIngressQuarantine(ctx context.Context,
	id string) error {

	return s.querier.DeleteIngressQuarantine(ctx, id)
}
