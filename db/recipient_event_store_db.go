package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/db/sqlc"
)

// RecipientEventInput describes a recipient output to persist.
type RecipientEventInput struct {
	// RecipientPkScript is the recipient script and event partition key.
	RecipientPkScript []byte

	// OutputIndex is the output index in the finalized Ark transaction.
	OutputIndex uint32

	// Value is the output amount in satoshis.
	Value int64
}

// RecipientEventStore is a DB-backed recipient event store implementation.
type RecipientEventStore struct {
	tx *TransactionExecutor[*sqlc.Queries]

	log btclog.Logger
}

// NewRecipientEventStore creates a new DB-backed recipient event store.
func NewRecipientEventStore(dbq BatchedQuerier,
	log btclog.Logger) *RecipientEventStore {

	if log == nil {
		log = btclog.Disabled
	}

	txExec := NewTransactionExecutor[*sqlc.Queries](
		dbq,
		func(tx *sql.Tx) *sqlc.Queries {
			return sqlc.NewWithBackend(tx, dbq.Backend())
		},
		log,
	)

	return &RecipientEventStore{
		tx:  txExec,
		log: log,
	}
}

// AppendRecipientEvents records per-recipient events for a finalized OOR
// session identified by its integer DB primary key.
func (s *RecipientEventStore) AppendRecipientEvents(ctx context.Context,
	sessionDBID int32, recipients []RecipientEventInput) error {

	if len(recipients) == 0 {
		return fmt.Errorf("recipients must be provided")
	}

	return s.tx.ExecTx(ctx, WriteTxOption(),
		func(q *sqlc.Queries) error {
			nextIDs := make(map[string]int64)
			createdAt := time.Now().UnixNano()

			for _, recipient := range recipients {
				err := appendRecipientEvent(
					ctx, q, sessionDBID, createdAt,
					recipient, nextIDs,
				)
				if err != nil {
					return err
				}
			}

			return nil
		},
	)
}

// appendRecipientEvent inserts a durable recipient event with a monotonic,
// per-recipient cursor.
//
// The INSERT uses unqualified ON CONFLICT DO NOTHING so that both the PK
// constraint (recipient_pk_script, event_id) and the idempotency constraint
// (recipient_pk_script, session_db_id, output_index) are absorbed without
// aborting a Postgres transaction. After a zero-row insert we distinguish
// an idempotent re-insert (skip) from a cursor collision (bump and retry)
// by re-reading the current max event_id.
func appendRecipientEvent(ctx context.Context, q *sqlc.Queries,
	sessionDBID int32, createdAt int64, recipient RecipientEventInput,
	nextIDs map[string]int64) error {

	if len(recipient.RecipientPkScript) == 0 {
		return fmt.Errorf("recipient pk script must be provided")
	}

	pkScript := recipient.RecipientPkScript
	key := string(pkScript)

	nextID, ok := nextIDs[key]
	if !ok {
		maxID, err := q.GetMaxOORRecipientEventID(ctx, pkScript)
		if err != nil {
			return err
		}

		nextID = maxID
	}

	nextID++

	outputIndex := int32(recipient.OutputIndex)
	value := recipient.Value

	for {
		// Keep a per-recipient monotonic cursor so clients can poll
		// using a stable "afterEventID" value and not miss events.
		params := sqlc.InsertOORRecipientEventParams{
			RecipientPkScript: pkScript,
			EventID:           nextID,
			SessionDbID:       sessionDBID,
			OutputIndex:       outputIndex,
			Value:             value,
			CreatedAt:         createdAt,
		}

		affected, err := q.InsertOORRecipientEvent(ctx, params)
		if err != nil {
			return err
		}

		if affected > 0 {
			nextIDs[key] = nextID

			return nil
		}

		// Zero rows affected: ON CONFLICT DO NOTHING absorbed a
		// constraint violation. Determine which constraint fired.
		//
		// Re-read the current max event_id. If it advanced past
		// our attempted nextID, a concurrent insert claimed the
		// cursor value (PK collision); bump and retry. Otherwise
		// the idempotency constraint
		// (pk_script, session_db_id, output_index) fired, meaning
		// this event was already persisted; skip silently.
		currentMax, err := q.GetMaxOORRecipientEventID(
			ctx, pkScript,
		)
		if err != nil {
			return err
		}

		if currentMax >= nextID {
			// Cursor collision — advance past the current max.
			nextID = currentMax + 1
			continue
		}

		// Idempotent re-insert for this session/output.
		nextIDs[key] = nextID

		return nil
	}
}

// ListRecipientEvents returns events addressed to the recipient after the
// provided cursor.
func (s *RecipientEventStore) ListRecipientEvents(ctx context.Context,
	recipientPkScript []byte, afterEventID int64, limit int32) (
	[]sqlc.OorRecipientEvent, error) {

	if len(recipientPkScript) == 0 {
		return nil, fmt.Errorf("recipient pk script must be provided")
	}

	if limit <= 0 {
		return nil, fmt.Errorf("limit must be positive")
	}

	var events []sqlc.OorRecipientEvent

	err := s.tx.ExecTx(ctx, ReadTxOption(),
		func(q *sqlc.Queries) error {
			rows, err := q.ListOORRecipientEventsAfter(
				ctx,
				sqlc.ListOORRecipientEventsAfterParams{
					RecipientPkScript: recipientPkScript,
					EventID:           afterEventID,
					Limit:             limit,
				},
			)
			if err != nil {
				return err
			}

			events = rows

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return events, nil
}
