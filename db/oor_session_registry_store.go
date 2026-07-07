package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

var (
	// ErrOORSessionNotFound indicates the OOR session registry row does not
	// exist.
	ErrOORSessionNotFound = errors.New("oor session registry row not found")
)

// OORSessionDirection records whether a registered OOR session was locally
// sent or received. Values are append-only; the numeric meaning of an existing
// value must never shift.
type OORSessionDirection int32

const (
	// OORSessionDirectionOutgoing marks a locally sent OOR session.
	OORSessionDirectionOutgoing OORSessionDirection = iota + 1

	// OORSessionDirectionIncoming marks a locally received OOR session.
	OORSessionDirectionIncoming
)

// OORSessionStatus is the coordinator-facing status of one OOR session. Values
// are append-only.
type OORSessionStatus int32

const (
	// OORSessionStatusPending means the session is still in flight.
	OORSessionStatusPending OORSessionStatus = iota

	// OORSessionStatusCompleted means the session completed successfully.
	OORSessionStatusCompleted

	// OORSessionStatusFailed means the session failed terminally.
	OORSessionStatusFailed
)

// IsTerminal reports whether the session status is terminal.
func (s OORSessionStatus) IsTerminal() bool {
	return s == OORSessionStatusCompleted || s == OORSessionStatusFailed
}

// OORSessionRegistryRecord is one OOR session's full durable state: the
// queryable control-plane fields plus the opaque resume snapshot. It is the
// single source of truth for the session -- the per-session actor reads and
// writes it directly inside its Read/Stage/Commit phases rather than using the
// generic actor-delivery fsm_checkpoints blob.
type OORSessionRegistryRecord struct {
	// SessionID is the 32-byte OOR session identifier.
	SessionID chainhash.Hash

	// ActorID is the durable per-session actor mailbox id.
	ActorID string

	// Direction records whether the session is outgoing or incoming.
	Direction OORSessionDirection

	// Phase is the latest control-plane phase string.
	Phase string

	// IdempotencyKey dedups a repeated outgoing StartTransferRequest. Empty
	// means no key (always empty for incoming sessions).
	IdempotencyKey string

	// Status is the coordinator-facing session status.
	Status OORSessionStatus

	// LastError is the latest terminal failure reason.
	LastError string

	// SnapshotData is the TLV-encoded per-session resume snapshot. Nil only
	// in the brief admission window before the first staged write.
	SnapshotData []byte

	// SnapshotVersion is the encoding version of SnapshotData.
	SnapshotVersion int32

	// CreatedAt is when the row was first written.
	CreatedAt time.Time

	// UpdatedAt is when the row was last updated.
	UpdatedAt time.Time
}

// OORSessionRegistryStoreDB bridges the OOR session registry control-plane to
// the sqlc-generated queries. Every method wraps the query in ExecTx so that,
// when ctx carries a durable-actor transaction (actor.TxFromContext), the write
// joins that outer tx and commits atomically alongside the mailbox ack; from
// the registry actor (no ambient tx) it opens its own short transaction.
type OORSessionRegistryStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]

	clock clock.Clock
}

// NewOORSessionRegistryStore creates an OOR session registry store from a
// Store.
func NewOORSessionRegistryStore(store *Store,
	clk clock.Clock) *OORSessionRegistryStoreDB {

	baseDB := store.BaseDB()

	txExec := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return store.Queries().WithTx(tx)
		},
		store.log,
	)

	return &OORSessionRegistryStoreDB{
		TransactionExecutor: txExec,
		clock:               clk,
	}
}

// UpsertSession persists or updates one OOR session registry row.
func (s *OORSessionRegistryStoreDB) UpsertSession(ctx context.Context,
	record OORSessionRegistryRecord) error {

	nowUnix := s.clock.Now().Unix()
	createdAt := record.CreatedAt.Unix()
	if record.CreatedAt.IsZero() {
		createdAt = nowUnix
	}

	return s.ExecTx(
		ctx, WriteTxOption(),
		func(q *sqlc.Queries) error {
			return q.UpsertOORSessionRegistry(
				ctx,
				sqlc.UpsertOORSessionRegistryParams{
					SessionID: record.SessionID[:],
					ActorID:   record.ActorID,
					Direction: int32(record.Direction),
					Phase:     record.Phase,
					IdempotencyKey: sql.NullString{
						String: record.IdempotencyKey,
						Valid: record.IdempotencyKey !=
							"",
					},
					Status: int32(record.Status),
					LastError: sql.NullString{
						String: record.LastError,
						Valid:  record.LastError != "",
					},
					SnapshotData:    record.SnapshotData,
					SnapshotVersion: record.SnapshotVersion,
					CreatedAt:       createdAt,
					UpdatedAt:       nowUnix,
				},
			)
		},
	)
}

// GetSession loads one OOR session registry row by session id.
func (s *OORSessionRegistryStoreDB) GetSession(ctx context.Context,
	sessionID chainhash.Hash) (*OORSessionRegistryRecord, error) {

	var record *OORSessionRegistryRecord

	readFn := func(q *sqlc.Queries) error {
		row, err := q.GetOORSessionRegistry(ctx, sessionID[:])
		if err != nil {

			// Let sql.ErrNoRows propagate so ExecTx recognises this
			// as a benign negative lookup; the sentinel translation
			// happens below.
			return err
		}

		converted, err := oorSessionRecordFromRow(row)
		if err != nil {
			return err
		}

		record = &converted

		return nil
	}

	err := s.ExecTx(ctx, ReadTxOption(), readFn)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOORSessionNotFound
	}
	if err != nil {
		return nil, err
	}

	return record, nil
}

// LookupActiveSessionByIdempotencyKey loads the non-failed OOR session
// registry row carrying the given outgoing idempotency key, if any. Failed
// sessions are excluded so a keyed retry after a failure admits a fresh
// session instead of deduping against the dead one; pending and completed
// sessions still answer for the key.
func (s *OORSessionRegistryStoreDB) LookupActiveSessionByIdempotencyKey(
	ctx context.Context, key string) (*OORSessionRegistryRecord, error) {

	if key == "" {
		return nil, ErrOORSessionNotFound
	}

	var record *OORSessionRegistryRecord

	readFn := func(q *sqlc.Queries) error {
		row, err := q.LookupActiveOORSessionRegistryByIdempotencyKey(
			ctx, sql.NullString{
				String: key,
				Valid:  true,
			},
		)
		if err != nil {
			return err
		}

		converted, err := oorSessionRecordFromRow(row)
		if err != nil {
			return err
		}

		record = &converted

		return nil
	}

	err := s.ExecTx(ctx, ReadTxOption(), readFn)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOORSessionNotFound
	}
	if err != nil {
		return nil, err
	}

	return record, nil
}

// ListNonTerminal loads every non-terminal OOR session registry row. The
// registry actor uses this on boot to respawn in-flight per-session actors.
func (s *OORSessionRegistryStoreDB) ListNonTerminal(ctx context.Context) (
	[]OORSessionRegistryRecord, error) {

	result := make([]OORSessionRegistryRecord, 0)

	readFn := func(q *sqlc.Queries) error {
		rows, err := q.ListNonTerminalOORSessionRegistry(ctx)
		if err != nil {
			return err
		}

		result = make([]OORSessionRegistryRecord, 0, len(rows))
		for i := range rows {
			converted, convErr := oorSessionRecordFromRow(rows[i])
			if convErr != nil {
				return convErr
			}

			result = append(result, converted)
		}

		return nil
	}

	if err := s.ExecTx(ctx, ReadTxOption(), readFn); err != nil {
		return nil, err
	}

	return result, nil
}

// ListSessions returns every OOR session registry row, terminal and
// non-terminal alike, for coarse diagnostic listings.
func (s *OORSessionRegistryStoreDB) ListSessions(ctx context.Context) (
	[]OORSessionRegistryRecord, error) {

	result := make([]OORSessionRegistryRecord, 0)

	readFn := func(q *sqlc.Queries) error {
		rows, err := q.ListAllOORSessionRegistry(ctx)
		if err != nil {
			return err
		}

		result = make([]OORSessionRegistryRecord, 0, len(rows))
		for i := range rows {
			converted, convErr := oorSessionRecordFromRow(rows[i])
			if convErr != nil {
				return convErr
			}

			result = append(result, converted)
		}

		return nil
	}

	if err := s.ExecTx(ctx, ReadTxOption(), readFn); err != nil {
		return nil, err
	}

	return result, nil
}

// oorSessionRecordFromRow converts a sqlc row into a domain record.
func oorSessionRecordFromRow(row sqlc.OorSessionRegistry) (
	OORSessionRegistryRecord, error) {

	if len(row.SessionID) != chainhash.HashSize {
		return OORSessionRegistryRecord{}, fmt.Errorf("unexpected "+
			"session id length %d", len(row.SessionID))
	}

	var sessionID chainhash.Hash
	copy(sessionID[:], row.SessionID)

	record := OORSessionRegistryRecord{
		SessionID:       sessionID,
		ActorID:         row.ActorID,
		Direction:       OORSessionDirection(row.Direction),
		Phase:           row.Phase,
		Status:          OORSessionStatus(row.Status),
		SnapshotData:    row.SnapshotData,
		SnapshotVersion: row.SnapshotVersion,
		CreatedAt:       time.Unix(row.CreatedAt, 0),
		UpdatedAt:       time.Unix(row.UpdatedAt, 0),
	}

	if row.IdempotencyKey.Valid {
		record.IdempotencyKey = row.IdempotencyKey.String
	}

	if row.LastError.Valid {
		record.LastError = row.LastError.String
	}

	return record, nil
}
