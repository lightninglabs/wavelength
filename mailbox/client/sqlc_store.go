package mailboxclient

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/lightninglabs/darepo-client/db/sqlc"
)

// SQLCStore is a Store implementation backed by the project's SQLC queries.
//
// This store is intended for production use, where the client already depends
// on the project's SQL database stack.
type SQLCStore struct {
	q *sqlc.Queries
}

// NewSQLCStore constructs a Store backed by the given SQLC Queries instance.
func NewSQLCStore(q *sqlc.Queries) *SQLCStore {
	return &SQLCStore{q: q}
}

// LoadCursor returns the persisted cursor for mailboxID.
func (s *SQLCStore) LoadCursor(ctx context.Context, mailboxID string) (
	uint64, error) {

	if s == nil || s.q == nil {
		return 0, errors.New("sqlc store is not initialized")
	}

	cursor, err := s.q.MailboxRPCClientGetCursor(ctx, mailboxID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}

		return 0, err
	}

	if cursor < 0 {
		return 0, fmt.Errorf("invalid cursor %d", cursor)
	}

	return uint64(cursor), nil
}

// SaveCursor persists cursor for mailboxID, treating it as monotonic.
func (s *SQLCStore) SaveCursor(ctx context.Context, mailboxID string,
	cursor uint64) error {

	if s == nil || s.q == nil {
		return errors.New("sqlc store is not initialized")
	}

	if cursor > math.MaxInt64 {
		return fmt.Errorf("cursor %d overflows int64", cursor)
	}

	return s.q.MailboxRPCClientUpsertCursor(
		ctx,
		sqlc.MailboxRPCClientUpsertCursorParams{
			MailboxID: mailboxID,
			Cursor:    int64(cursor),
		},
	)
}

// PutResponse records payload for correlationID if it doesn't already exist.
func (s *SQLCStore) PutResponse(ctx context.Context, mailboxID string,
	correlationID string, payload []byte) error {

	if s == nil || s.q == nil {
		return errors.New("sqlc store is not initialized")
	}

	return s.q.MailboxRPCClientPutResponse(
		ctx,
		sqlc.MailboxRPCClientPutResponseParams{
			MailboxID:     mailboxID,
			CorrelationID: correlationID,
			Payload:       payload,
		},
	)
}

// GetResponse returns payload for correlationID if present.
func (s *SQLCStore) GetResponse(ctx context.Context, mailboxID string,
	correlationID string) ([]byte, bool, error) {

	if s == nil || s.q == nil {
		return nil, false, errors.New("sqlc store is not initialized")
	}

	payload, err := s.q.MailboxRPCClientGetResponse(
		ctx,
		sqlc.MailboxRPCClientGetResponseParams{
			MailboxID:     mailboxID,
			CorrelationID: correlationID,
		},
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}

		return nil, false, err
	}

	return payload, true, nil
}

// DeleteResponse removes payload for correlationID if present.
func (s *SQLCStore) DeleteResponse(ctx context.Context, mailboxID string,
	correlationID string) error {

	if s == nil || s.q == nil {
		return errors.New("sqlc store is not initialized")
	}

	return s.q.MailboxRPCClientDeleteResponse(
		ctx,
		sqlc.MailboxRPCClientDeleteResponseParams{
			MailboxID:     mailboxID,
			CorrelationID: correlationID,
		},
	)
}

var _ Store = (*SQLCStore)(nil)
