package oor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/db"
)

// DBIncomingCursorStore adapts db.OORArtifactPersistenceStore to the
// IncomingCursorStore interface expected by OORService.
type DBIncomingCursorStore struct {
	store *db.OORArtifactPersistenceStore
}

// NewDBIncomingCursorStore constructs a cursor-store adapter from the DB OOR
// artifact store.
func NewDBIncomingCursorStore(
	store *db.OORArtifactPersistenceStore) *DBIncomingCursorStore {

	return &DBIncomingCursorStore{store: store}
}

// ListOwnedReceiveScripts returns all tracked receive scripts.
func (s *DBIncomingCursorStore) ListOwnedReceiveScripts(
	ctx context.Context) ([]OwnedReceiveScript, error) {

	if s == nil || s.store == nil {
		return nil, fmt.Errorf("store must be provided")
	}

	rows, err := s.store.ListOwnedReceiveScripts(ctx)
	if err != nil {
		return nil, err
	}

	scripts := make([]OwnedReceiveScript, 0, len(rows))
	for i := range rows {
		scripts = append(scripts, OwnedReceiveScript{
			PkScript: rows[i].PkScript,
		})
	}

	return scripts, nil
}

// GetRecipientCursor returns one script cursor if present.
func (s *DBIncomingCursorStore) GetRecipientCursor(ctx context.Context,
	recipientPkScript []byte) (*RecipientCursor, error) {

	if s == nil || s.store == nil {
		return nil, fmt.Errorf("store must be provided")
	}

	row, err := s.store.GetRecipientCursor(ctx, recipientPkScript)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}

		return nil, err
	}

	var sessionID *SessionID
	if len(row.LastSessionID) > 0 {
		hash, err := parseSessionHash(row.LastSessionID)
		if err != nil {
			return nil, err
		}

		session := SessionID(*hash)
		sessionID = &session
	}

	return &RecipientCursor{
		RecipientPkScript: row.RecipientPkScript,
		LastEventID:       row.LastEventID,
		LastSessionID:     sessionID,
	}, nil
}

// UpsertRecipientCursor stores the latest processed cursor for one script.
func (s *DBIncomingCursorStore) UpsertRecipientCursor(ctx context.Context,
	recipientPkScript []byte, lastEventID int64,
	lastSessionID *SessionID) error {

	if s == nil || s.store == nil {
		return fmt.Errorf("store must be provided")
	}

	var sessionHash *chainhash.Hash
	if lastSessionID != nil {
		hash := chainhash.Hash(*lastSessionID)
		sessionHash = &hash
	}

	return s.store.UpsertRecipientCursor(ctx, recipientPkScript,
		lastEventID, sessionHash)
}

// parseSessionHash validates and converts a session-id byte slice to a hash.
func parseSessionHash(raw []byte) (*chainhash.Hash, error) {
	if len(raw) != chainhash.HashSize {
		return nil, fmt.Errorf(
			"invalid session id length: %d", len(raw),
		)
	}

	hash, err := chainhash.NewHash(raw)
	if err != nil {
		return nil, err
	}

	return hash, nil
}

var _ IncomingCursorStore = (*DBIncomingCursorStore)(nil)
