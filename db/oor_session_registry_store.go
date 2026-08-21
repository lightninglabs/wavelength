package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/rpc/oorpb"
	"github.com/lightningnetwork/lnd/clock"
)

var (
	// ErrOORSessionNotFound indicates the OOR session registry row does not
	// exist.
	ErrOORSessionNotFound = errors.New("oor session registry row not found")

	// ErrOORDispatchAttemptNotFound indicates that no durable outgoing
	// dispatch is bound to the requested key or session.
	ErrOORDispatchAttemptNotFound = errors.New("oor dispatch attempt not " +
		"found")

	// ErrOORDispatchAttemptConflict indicates that a dispatch key or
	// session is already bound to different canonical request data.
	ErrOORDispatchAttemptConflict = errors.New("oor dispatch attempt " +
		"conflict")
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

	// DispatchRequestData is the canonical outgoing request and result map
	// written at the first submit-capable checkpoint. UpsertSession stores
	// it in the immutable dispatch-attempt table, not the mutable session
	// row. Later checkpoints and every incoming lifecycle leave it empty.
	DispatchRequestData []byte

	// FlowVersion is the permanent OOR flow version this session was
	// conducted under (distinct from SnapshotVersion, which versions only
	// the resume blob's encoding). Stamped at creation and validated on
	// load. Until a second flow exists it is always FlowVersionV1.
	FlowVersion oorpb.FlowVersion

	// CreatedAt is when the row was first written.
	CreatedAt time.Time

	// UpdatedAt is when the row was last updated.
	UpdatedAt time.Time
}

// OORDispatchAttemptRecord binds one caller key to the exact outgoing request
// that was durably admitted for transport. The record is immutable and remains
// authoritative after the session row changes direction or becomes terminal.
type OORDispatchAttemptRecord struct {
	// IdempotencyKey is the caller-owned global dispatch identity.
	IdempotencyKey string

	// SessionID is the deterministic OOR transaction identity.
	SessionID chainhash.Hash

	// RequestData is the normalized recipient request plus output
	// positions. Legacy backfilled records leave it empty and therefore
	// fail closed when a caller needs exact recipient reconciliation.
	RequestData []byte

	// CreatedAt is when the binding became durable.
	CreatedAt time.Time
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

	params := sqlc.UpsertOORSessionRegistryParams{
		SessionID: record.SessionID[:],
		ActorID:   record.ActorID,
		Direction: int32(record.Direction),
		Phase:     record.Phase,
		IdempotencyKey: sql.NullString{
			String: record.IdempotencyKey,
			Valid:  record.IdempotencyKey != "",
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

		// flow_version is write-once (not in the ON CONFLICT update
		// set); the oor package stamps the value and applies the load
		// guard, since db cannot import oor.
		FlowVersion: int32(record.FlowVersion),
	}

	return s.ExecTx(
		ctx, WriteTxOption(),
		func(q *sqlc.Queries) error {
			if err := q.UpsertOORSessionRegistry(
				ctx, params,
			); err != nil {
				return err
			}

			if record.Direction != OORSessionDirectionOutgoing ||
				record.IdempotencyKey == "" ||
				len(record.DispatchRequestData) == 0 {
				return nil
			}

			return ensureOORDispatchAttempt(
				ctx, q, record.IdempotencyKey, record.SessionID,
				record.DispatchRequestData, nowUnix,
			)
		},
	)
}

// ensureOORDispatchAttempt inserts the immutable dispatch binding or proves
// that an idempotent redelivery matches the row that already won. The insert
// and comparison run inside the session checkpoint transaction, so a conflict
// rolls back both the session advance and its transport enqueue.
func ensureOORDispatchAttempt(ctx context.Context, q *sqlc.Queries, key string,
	sessionID chainhash.Hash, requestData []byte, createdAt int64) error {

	err := q.InsertOORDispatchAttempt(
		ctx, sqlc.InsertOORDispatchAttemptParams{
			IdempotencyKey: key,
			SessionID:      sessionID[:],
			RequestData:    requestData,
			CreatedAt:      createdAt,
		},
	)
	if err != nil {
		return err
	}

	winner, err := q.GetOORDispatchAttemptByIdempotencyKey(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		// The insert can lose on the unique session_id constraint while
		// the requested key remains absent. Report the same typed
		// conflict as a key collision instead of leaking sql.ErrNoRows.
		winnerBySession, sessionErr :=
			q.GetOORDispatchAttemptBySessionID(ctx, sessionID[:])
		if sessionErr == nil {
			return fmt.Errorf("%w: session %s is already bound "+
				"to key %q", ErrOORDispatchAttemptConflict,
				sessionID.String(),
				winnerBySession.IdempotencyKey)
		}
		if !errors.Is(sessionErr, sql.ErrNoRows) {
			return sessionErr
		}
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(winner.SessionID, sessionID[:]) ||
		!bytes.Equal(winner.RequestData, requestData) {
		return fmt.Errorf("%w: idempotency key %q",
			ErrOORDispatchAttemptConflict, key)
	}

	return nil
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

// GetDispatchAttemptByIdempotencyKey loads the immutable outgoing dispatch
// bound to key. Unlike the mutable session lookup, it remains valid across
// terminal and incoming lifecycle updates.
func (s *OORSessionRegistryStoreDB) GetDispatchAttemptByIdempotencyKey(
	ctx context.Context, key string) (*OORDispatchAttemptRecord, error) {

	if key == "" {
		return nil, ErrOORDispatchAttemptNotFound
	}

	var record *OORDispatchAttemptRecord
	readFn := func(q *sqlc.Queries) error {
		row, err := q.GetOORDispatchAttemptByIdempotencyKey(ctx, key)
		if err != nil {
			return err
		}

		converted, err := oorDispatchAttemptFromRow(row)
		if err != nil {
			return err
		}

		record = &converted

		return nil
	}

	err := s.ExecTx(ctx, ReadTxOption(), readFn)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOORDispatchAttemptNotFound
	}
	if err != nil {
		return nil, err
	}

	return record, nil
}

// GetDispatchAttemptBySessionID loads the immutable outgoing dispatch for one
// deterministic OOR session.
func (s *OORSessionRegistryStoreDB) GetDispatchAttemptBySessionID(
	ctx context.Context, sessionID chainhash.Hash) (
	*OORDispatchAttemptRecord, error) {

	var record *OORDispatchAttemptRecord
	readFn := func(q *sqlc.Queries) error {
		row, err := q.GetOORDispatchAttemptBySessionID(
			ctx, sessionID[:],
		)
		if err != nil {
			return err
		}

		converted, err := oorDispatchAttemptFromRow(row)
		if err != nil {
			return err
		}

		record = &converted

		return nil
	}

	err := s.ExecTx(ctx, ReadTxOption(), readFn)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOORDispatchAttemptNotFound
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
		FlowVersion:     oorpb.FlowVersion(row.FlowVersion),
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

// oorDispatchAttemptFromRow validates and converts one generated dispatch row.
func oorDispatchAttemptFromRow(row sqlc.OorDispatchAttempt) (
	OORDispatchAttemptRecord, error) {

	if len(row.SessionID) != chainhash.HashSize {
		return OORDispatchAttemptRecord{}, fmt.Errorf("unexpected "+
			"dispatch session id length %d", len(row.SessionID))
	}

	var sessionID chainhash.Hash
	copy(sessionID[:], row.SessionID)

	return OORDispatchAttemptRecord{
		IdempotencyKey: row.IdempotencyKey,
		SessionID:      sessionID,
		RequestData:    row.RequestData,
		CreatedAt:      time.Unix(row.CreatedAt, 0),
	}, nil
}
