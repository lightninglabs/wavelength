package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/mailbox"
	"google.golang.org/protobuf/proto"
)

// MailboxEnvelopeStore is a DB-backed implementation of mailbox.Store
// that persists envelopes using sqlc-generated queries. It supports
// both SQLite and Postgres backends through the unified sqlc
// abstraction and integrates with the TransactionExecutor for atomic
// envelope+state commits.
type MailboxEnvelopeStore struct {
	tx *TransactionExecutor[*sqlc.Queries]

	cfg mailbox.StoreConfig
	log btclog.Logger

	notifyMu sync.Mutex
	notify   map[string]chan struct{}
}

// NewMailboxEnvelopeStore creates a new DB-backed mailbox store using
// the provided batched querier for transaction support.
func NewMailboxEnvelopeStore(dbq BatchedQuerier, log btclog.Logger,
	opts ...mailbox.StoreOption) *MailboxEnvelopeStore {

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

	cfg := mailbox.DefaultStoreConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	return &MailboxEnvelopeStore{
		tx:     txExec,
		cfg:    cfg,
		log:    log,
		notify: make(map[string]chan struct{}),
	}
}

// Append serializes the envelope to proto bytes and inserts it into
// the database. The assigned event sequence number is returned.
func (s *MailboxEnvelopeStore) Append(ctx context.Context,
	env *mailbox.Envelope) (uint64, error) {

	if env == nil {
		return 0, fmt.Errorf("missing envelope")
	}
	if env.Recipient == "" {
		return 0, fmt.Errorf("missing recipient")
	}
	if env.MsgId == "" {
		return 0, fmt.Errorf("missing msg_id")
	}

	// Serialize the full envelope to proto bytes for storage.
	envBytes, err := proto.Marshal(env)
	if err != nil {
		return 0, fmt.Errorf("marshal envelope: %w", err)
	}

	if s.cfg.MaxEnvelopeBytes > 0 {
		if len(envBytes) > s.cfg.MaxEnvelopeBytes {
			return 0, &mailbox.ErrEnvelopeTooLarge{
				Size: len(envBytes),
				Max:  s.cfg.MaxEnvelopeBytes,
			}
		}
	}

	var seq int64
	dbErr := s.tx.ExecTx(
		ctx, WriteTxOption(),
		func(q *sqlc.Queries) error {
			// Enforce per-mailbox envelope count limit.
			//
			// NOTE: Under concurrent writers targeting the
			// same recipient, the COUNT+INSERT is not
			// serialized, so two transactions may both
			// observe count=max-1 and both insert,
			// temporarily exceeding the cap by one. This
			// is acceptable for the current use case where
			// each recipient has a single writer.
			if s.cfg.MaxEnvelopesPerMailbox > 0 {
				count, countErr := q.CountMailboxEnvelopes(
					ctx, env.Recipient,
				)
				if countErr != nil {
					return fmt.Errorf("count envelopes: %w",
						countErr)
				}

				maxPerMbox := s.cfg.MaxEnvelopesPerMailbox
				if int(count) >= maxPerMbox {
					return &mailbox.ErrMailboxFull{
						Recipient: env.Recipient,
						Max:       maxPerMbox,
					}
				}
			}

			var appendErr error
			seq, appendErr = q.AppendMailboxEnvelope(
				ctx, sqlc.AppendMailboxEnvelopeParams{
					Recipient: env.Recipient,
					MsgID:     env.MsgId,
					Envelope:  envBytes,
					CreatedAt: time.Now().UnixNano(),
				},
			)

			// ON CONFLICT DO NOTHING returns
			// sql.ErrNoRows when the msg_id already
			// exists. Treat as idempotent success.
			if errors.Is(appendErr, sql.ErrNoRows) {
				seq = 0

				return nil
			}

			return appendErr
		},
	)
	if dbErr != nil {
		return 0, fmt.Errorf("append envelope: %w", dbErr)
	}

	s.log.DebugS(ctx, "Appended envelope",
		slog.String("recipient", env.Recipient),
		slog.Int64("seq", seq),
	)

	if seq > 0 {
		s.notifyPullers(env.Recipient)
	}

	return uint64(seq), nil
}

// Pull returns up to limit envelopes for a recipient starting at the
// given cursor. If no envelopes are available, it polls at the
// configured PullPollInterval until the context expires.
// Appends in the same process wake waiters immediately; the polling fallback
// preserves cross-process visibility where no in-memory signal exists.
func (s *MailboxEnvelopeStore) Pull(ctx context.Context, recipient string,
	cursor uint64, limit int) ([]*mailbox.Envelope, uint64, error) {

	if recipient == "" {
		return nil, 0, fmt.Errorf("missing recipient")
	}
	if limit <= 0 {
		return nil, 0, fmt.Errorf("limit must be positive")
	}

	// Use a reusable timer to avoid per-iteration allocations.
	pollTimer := time.NewTimer(s.cfg.PullPollInterval)
	defer pollTimer.Stop()

	for {
		notify := s.pullNotifyChan(recipient)

		var rows []sqlc.MailboxEnvelope
		dbErr := s.tx.ExecTx(
			ctx, ReadTxOption(),
			func(q *sqlc.Queries) error {
				var err error
				rows, err = q.PullMailboxEnvelopes(
					ctx,
					sqlc.PullMailboxEnvelopesParams{
						Recipient: recipient,
						EventSeq:  int64(cursor),
						Limit:     int32(limit),
					},
				)

				return err
			},
		)
		if dbErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, 0, ctxErr
			}

			return nil, 0, fmt.Errorf("pull envelopes: %w", dbErr)
		}

		if len(rows) > 0 {
			return s.rowsToEnvelopes(rows)
		}

		// No envelopes available. Wait for the poll interval
		// or context cancellation. Drain the timer before
		// resetting to avoid a stale value from a previous
		// iteration where the DB query took longer than the
		// poll interval.
		if !pollTimer.Stop() {
			select {
			case <-pollTimer.C:
			default:
			}
		}
		pollTimer.Reset(s.cfg.PullPollInterval)

		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()

		case <-notify:
		case <-pollTimer.C:
		}
	}
}

// pullNotifyChan returns the current in-process notification channel for a
// recipient. The channel is created before the empty pull query so an append
// racing after that query can wake the waiter without waiting for the poll
// interval.
func (s *MailboxEnvelopeStore) pullNotifyChan(
	recipient string) <-chan struct{} {

	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()

	ch := s.notify[recipient]
	if ch == nil {
		ch = make(chan struct{})
		s.notify[recipient] = ch
	}

	return ch
}

// notifyPullers wakes in-process pullers waiting on recipient. Pullers that
// start after this point will observe the row through the DB query path.
func (s *MailboxEnvelopeStore) notifyPullers(recipient string) {
	s.notifyMu.Lock()
	ch := s.notify[recipient]
	delete(s.notify, recipient)
	s.notifyMu.Unlock()

	if ch != nil {
		close(ch)
	}
}

// rowsToEnvelopes deserializes database rows into proto Envelope
// pointers and computes the next cursor.
func (s *MailboxEnvelopeStore) rowsToEnvelopes(rows []sqlc.MailboxEnvelope) (
	[]*mailbox.Envelope, uint64, error) {

	envelopes := make([]*mailbox.Envelope, 0, len(rows))

	for _, row := range rows {
		env := &mailbox.Envelope{}
		if err := proto.Unmarshal(row.Envelope, env); err != nil {
			return nil, 0, fmt.Errorf("unmarshal envelope "+
				"seq=%d: %w", row.EventSeq, err)
		}

		// Restore the event_seq from the DB row since it is
		// assigned by the database, not the caller.
		env.EventSeq = uint64(row.EventSeq)

		envelopes = append(envelopes, env)
	}

	// Next cursor is one past the last returned sequence.
	nextCursor := envelopes[len(envelopes)-1].EventSeq + 1

	return envelopes, nextCursor, nil
}

// AckUpTo advances the ack cursor for a recipient and
// garbage-collects envelopes below the new cursor. The cursor is
// monotonic: attempts to decrease it are treated as no-ops by the
// database UPSERT.
func (s *MailboxEnvelopeStore) AckUpTo(ctx context.Context, recipient string,
	cursor uint64) error {

	if recipient == "" {
		return fmt.Errorf("missing recipient")
	}

	var deleted int64
	dbErr := s.tx.ExecTx(
		ctx, WriteTxOption(),
		func(q *sqlc.Queries) error {
			// Upsert the ack cursor (monotonic: DB handles
			// the max check).
			err := q.UpsertMailboxAckCursor(
				ctx, sqlc.UpsertMailboxAckCursorParams{
					Recipient: recipient,
					AckCursor: int64(cursor),
				},
			)
			if err != nil {
				return fmt.Errorf("upsert ack cursor: %w", err)
			}

			// Garbage-collect acknowledged envelopes.
			deleted, err = q.DeleteAckedMailboxEnvelopes(
				ctx,
				sqlc.DeleteAckedMailboxEnvelopesParams{
					Recipient: recipient,
					EventSeq:  int64(cursor),
				},
			)

			return err
		},
	)
	if dbErr != nil {
		return fmt.Errorf("ack up to: %w", dbErr)
	}

	if deleted > 0 {
		s.log.DebugS(ctx, "Acked and GC'd envelopes",
			slog.String("recipient", recipient),
			slog.Uint64("cursor", cursor),
			slog.Int64("deleted", deleted),
		)
	}

	return nil
}

// Compile-time interface check.
var _ mailbox.Store = (*MailboxEnvelopeStore)(nil)
