package oor

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btclog/v2"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

// DBRecipientEventStore is a DB-backed RecipientEventStore implementation.
type DBRecipientEventStore struct {
	store *db.RecipientEventStore

	sessions SessionStore
}

// NewDBRecipientEventStore creates a new DB-backed recipient event store.
func NewDBRecipientEventStore(dbq db.BatchedQuerier, clk clock.Clock,
	log btclog.Logger) *DBRecipientEventStore {

	return &DBRecipientEventStore{
		store:    db.NewRecipientEventStore(dbq, log),
		sessions: NewDBSessionStore(dbq, clk, log),
	}
}

// AppendRecipientEvents records per-recipient events for the finalized Ark
// transaction. The session_id is resolved to the integer DB primary key
// before inserting events.
func (s *DBRecipientEventStore) AppendRecipientEvents(ctx context.Context,
	sessionID SessionID, arkPSBT *psbt.Packet,
	recipients []clientoor.ArkRecipientOutput) error {

	if sessionID == (SessionID{}) {
		return fmt.Errorf("session id must be provided")
	}

	if arkPSBT == nil {
		return fmt.Errorf("ark psbt must be provided")
	}

	if len(recipients) == 0 {
		return fmt.Errorf("recipients must be provided")
	}

	// Resolve the session integer DB ID from the session_id hash.
	sessionDBID, err := s.resolveSessionDBID(ctx, sessionID)
	if err != nil {
		return err
	}

	inputs := make([]db.RecipientEventInput, len(recipients))
	for i, recipient := range recipients {
		inputs[i] = db.RecipientEventInput{
			RecipientPkScript: recipient.PkScript,
			OutputIndex:       recipient.OutputIndex,
			Value:             int64(recipient.Value),
		}
	}

	return s.store.AppendRecipientEvents(ctx, sessionDBID, inputs)
}

// ListRecipientEvents returns events addressed to the recipient after the
// provided cursor.
func (s *DBRecipientEventStore) ListRecipientEvents(ctx context.Context,
	recipientPkScript []byte, afterEventID int64,
	limit int32) ([]*RecipientEvent, error) {

	rows, err := s.store.ListRecipientEvents(
		ctx, recipientPkScript, afterEventID, limit,
	)
	if err != nil {
		return nil, err
	}

	events := make([]*RecipientEvent, 0, len(rows))

	// Cache loaded packages by session DB ID to avoid repeated lookups
	// when multiple events reference the same session.
	loadedPackages := make(map[int32]*loadedSession)

	for _, row := range rows {
		loaded, ok := loadedPackages[row.SessionDbID]
		if !ok {
			loaded, err = s.loadSessionForEvent(
				ctx, row.SessionDbID,
			)
			if err != nil {
				return nil, err
			}

			loadedPackages[row.SessionDbID] = loaded
		}

		event := decodeRecipientEventRow(
			row, loaded.sessionID, loaded.pkg,
		)

		events = append(events, event)
	}

	return events, nil
}

// loadedSession caches session metadata needed for event decoding.
type loadedSession struct {
	sessionID SessionID
	pkg       *FinalizedPackage
}

// resolveSessionDBID looks up the integer DB primary key for a session_id.
func (s *DBRecipientEventStore) resolveSessionDBID(ctx context.Context,
	sessionID SessionID) (int32, error) {

	// Use LoadFinalizedPackage's underlying session lookup. We access
	// the sessions store to get the session row which includes the DB
	// ID. Since SessionStore doesn't expose GetSession directly, we
	// use a type assertion on the concrete store.
	dbStore, ok := s.sessions.(*DBSessionStore)
	if !ok {
		return 0, fmt.Errorf("session store does not support " +
			"DB ID resolution")
	}

	var dbID int32

	err := dbStore.tx.ExecTx(ctx, db.ReadTxOption(),
		func(q *sqlc.Queries) error {
			row, err := q.GetOORSession(
				ctx, sessionIDBytes(sessionID),
			)
			if err != nil {
				return fmt.Errorf("session not found: "+
					"%s: %w", sessionID, err)
			}

			dbID = int32(row.ID)

			return nil
		},
	)

	return dbID, err
}

// loadSessionForEvent loads the session ID and finalized package for a given
// session DB ID.
func (s *DBRecipientEventStore) loadSessionForEvent(ctx context.Context,
	sessionDBID int32) (*loadedSession, error) {

	dbStore, ok := s.sessions.(*DBSessionStore)
	if !ok {
		return nil, fmt.Errorf("session store does not support " +
			"DB ID resolution")
	}

	var loaded loadedSession

	err := dbStore.tx.ExecTx(ctx, db.ReadTxOption(),
		func(q *sqlc.Queries) error {
			row, err := q.GetOORSessionByID(
				ctx, int64(sessionDBID),
			)
			if err != nil {
				return fmt.Errorf("session db id %d "+
					"not found: %w", sessionDBID, err)
			}

			idHash, err := parseSessionHash(row.SessionID)
			if err != nil {
				return err
			}

			loaded.sessionID = idHash

			arkPSBT, err := deserializePSBT(row.ArkPsbt)
			if err != nil {
				return err
			}

			checkpointRows, err := q.ListOORCheckpoints(
				ctx, sessionDBID,
			)
			if err != nil {
				return err
			}

			checkpoints := make(
				[]*psbt.Packet, 0, len(checkpointRows),
			)
			for i := range checkpointRows {
				pkt, err := deserializePSBT(
					checkpointRows[i].CheckpointPsbt,
				)
				if err != nil {
					return err
				}

				checkpoints = append(checkpoints, pkt)
			}

			loaded.pkg = &FinalizedPackage{
				ArkPSBT:              arkPSBT,
				FinalCheckpointPSBTs: checkpoints,
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &loaded, nil
}

// parseSessionHash parses a 32-byte session ID blob into a SessionID.
func parseSessionHash(b []byte) (SessionID, error) {
	if len(b) != 32 {
		return SessionID{}, fmt.Errorf(
			"invalid session id length: %d", len(b),
		)
	}

	var id SessionID
	copy(id[:], b)

	return id, nil
}

// decodeRecipientEventRow converts a db row into a typed recipient event.
func decodeRecipientEventRow(row sqlc.OorRecipientEvent,
	sessionID SessionID, pkg *FinalizedPackage) *RecipientEvent {

	return &RecipientEvent{
		EventID:              row.EventID,
		SessionID:            sessionID,
		OutputIndex:          uint32(row.OutputIndex),
		Value:                btcutil.Amount(row.Value),
		ArkPSBT:              pkg.ArkPSBT,
		FinalCheckpointPSBTs: pkg.FinalCheckpointPSBTs,
		CreatedAt:            time.Unix(0, row.CreatedAt),
		RecipientPkScript:    row.RecipientPkScript,
	}
}

var _ RecipientEventStore = (*DBRecipientEventStore)(nil)
