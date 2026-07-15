package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

// ActivityStore groups the SQL methods needed to maintain the canonical
// activity log: the current-state activity_entries projection and the
// append-only activity_events transition log.
type ActivityStore interface {
	UpsertActivityEntry(ctx context.Context,
		arg sqlc.UpsertActivityEntryParams) (int64, error)

	AppendActivityEvent(ctx context.Context,
		arg sqlc.AppendActivityEventParams) (int64, error)

	GetActivityEntry(ctx context.Context,
		canonicalID string) (sqlc.ActivityEntry, error)

	CountActivityEntriesByStatus(ctx context.Context,
		status int64) (int64, error)

	ListActivityEntries(ctx context.Context,
		arg sqlc.ListActivityEntriesParams) (
		[]sqlc.ActivityEntry,
		error,
	)

	ListEntriesByKindStatus(ctx context.Context,
		arg sqlc.ListEntriesByKindStatusParams) (
		[]sqlc.ActivityEntry,
		error,
	)

	PullActivityEvents(ctx context.Context,
		arg sqlc.PullActivityEventsParams) ([]sqlc.ActivityEvent, error)
}

// BatchedActivityStore combines the query surface with batched transaction
// execution.
type BatchedActivityStore interface {
	ActivityStore
	BatchedTx[ActivityStore]
}

// ActivityProjection is the projector's input: a normalized snapshot of one
// activity row at a single lifecycle transition. The persistence store maps it
// to both an activity_entries upsert (current state) and an activity_events
// append (the transition), so the projector never touches sqlc params.
//
// Empty BLOB handles MUST be passed as nil, never a zero-length slice, to avoid
// the Postgres BYTEA `x”` pitfall. ConfirmationHeight is nil until known.
type ActivityProjection struct {
	// CanonicalID is the stable identity used to update one activity row.
	CanonicalID string

	// Kind identifies the wallet activity category.
	Kind int64

	// Status is the lifecycle state being projected.
	Status int64

	// AmountSat is the signed activity amount in satoshis.
	AmountSat int64

	// FeeSat is the activity fee in satoshis.
	FeeSat int64

	// Counterparty identifies the activity destination or source.
	Counterparty string

	// Note is immutable user-facing context attached at admission.
	Note string

	// Phase identifies the current detailed lifecycle phase.
	Phase int64

	// PhaseLabel is the display label for Phase.
	PhaseLabel string

	// FailureCode identifies a terminal activity failure.
	FailureCode int64

	// FailureReason describes a terminal activity failure.
	FailureReason string

	// PendingStatus identifies the pending enum value used to reject stale
	// terminal-to-pending transitions.
	PendingStatus int64

	// PaymentHash correlates Lightning and credit payment activity.
	PaymentHash []byte

	// Txid identifies an on-chain activity transaction.
	Txid []byte

	// ConfirmationHeight is the on-chain confirmation height when known.
	ConfirmationHeight *int64

	// VtxoOutpoint identifies the Ark VTXO affected by this activity.
	VtxoOutpoint string

	// SwapSessionID correlates activity with its swap session.
	SwapSessionID []byte

	// LedgerTxid correlates activity with its credit ledger transaction.
	LedgerTxid []byte

	// BoardingAddr is the on-chain address used for a boarding deposit.
	BoardingAddr []byte

	// RequestJSON preserves the immutable activity request snapshot.
	RequestJSON string

	// EntryJSON is the protojson snapshot of the WalletEntry emitted at
	// this transition, stored verbatim on the activity_events row.
	EntryJSON string

	// CreatedAtUnix is the activity creation timestamp in Unix seconds.
	CreatedAtUnix int64

	// UpdatedAtUnix is the lifecycle update timestamp in Unix seconds.
	UpdatedAtUnix int64
}

// ActivityPersistenceStore persists the canonical activity log. ProjectEntry
// performs the atomic dual-write (upsert the current-state row, then append the
// transition row) that backs List and a resumable SubscribeWallet.
type ActivityPersistenceStore struct {
	db    BatchedActivityStore
	clock clock.Clock
}

// NewActivityPersistenceStore creates an activity-log store using the
// transaction executor pattern.
func NewActivityPersistenceStore(
	db BatchedActivityStore, clk clock.Clock,
) *ActivityPersistenceStore {

	return &ActivityPersistenceStore{
		db:    db,
		clock: clk,
	}
}

// ProjectEntry advances the activity row to the projected state and records the
// transition, atomically. The entry is upserted before the event is appended so
// the activity_events foreign key is satisfied within the same transaction. It
// returns the event_seq assigned to the appended transition, or 0 when the
// projection was change-suppressed (no transition, so nothing to emit).
func (s *ActivityPersistenceStore) ProjectEntry(ctx context.Context,
	p ActivityProjection) (int64, error) {

	// A row must carry a creation and update time. When the projection
	// omits them (a malformed or synthetic entry), fall back to the
	// injected clock so the row sorts at "now" rather than the unix epoch.
	now := s.clock.Now().Unix()
	createdAt := p.CreatedAtUnix
	if createdAt == 0 {
		createdAt = now
	}
	updatedAt := p.UpdatedAtUnix
	if updatedAt == 0 {
		updatedAt = now
	}

	var eventSeq int64
	err := s.db.ExecTx(ctx, WriteTxOption(), func(q ActivityStore) error {
		// Reset per attempt: ExecTx may retry the closure.
		eventSeq = 0

		// Only record a transition when the projection actually changes
		// the current-state row. Redundant re-emits — the startup
		// backfill on every wallet-ready start, and the swap monitor's
		// include-existing replay — must not append duplicate
		// activity_events rows, or a resumable subscriber would later
		// replay bogus transitions for states that never changed.
		existing, err := q.GetActivityEntry(ctx, p.CanonicalID)
		switch {
		case err == nil:
			if !p.changesRow(existing) {
				return nil
			}

		case errors.Is(err, sql.ErrNoRows):
			// New operation: insert the row and its first event.

		default:
			return err
		}

		rows, err := q.UpsertActivityEntry(
			ctx, sqlc.UpsertActivityEntryParams{
				CanonicalID:   p.CanonicalID,
				Kind:          p.Kind,
				Status:        p.Status,
				AmountSat:     p.AmountSat,
				FeeSat:        p.FeeSat,
				Counterparty:  p.Counterparty,
				Note:          p.Note,
				Phase:         p.Phase,
				PhaseLabel:    p.PhaseLabel,
				FailureCode:   p.FailureCode,
				FailureReason: p.FailureReason,
				PaymentHash:   p.PaymentHash,
				Txid:          p.Txid,
				ConfirmationHeight: nullInt64(
					p.ConfirmationHeight,
				),
				VtxoOutpoint:  p.VtxoOutpoint,
				SwapSessionID: p.SwapSessionID,
				LedgerTxid:    p.LedgerTxid,
				BoardingAddr:  p.BoardingAddr,
				RequestJson:   p.RequestJSON,
				CreatedAtUnix: createdAt,
				UpdatedAtUnix: updatedAt,
				PendingStatus: p.PendingStatus,
			},
		)
		if err != nil {
			return err
		}
		if rows == 0 {
			return nil
		}

		seq, err := q.AppendActivityEvent(
			ctx, sqlc.AppendActivityEventParams{
				CanonicalID:   p.CanonicalID,
				Status:        p.Status,
				Phase:         p.Phase,
				EntryJson:     p.EntryJSON,
				CreatedAtUnix: updatedAt,
			},
		)
		if err != nil {
			return err
		}
		eventSeq = seq

		return nil
	})

	return eventSeq, err
}

// GetEntry returns one current-state row by its canonical id.
func (s *ActivityPersistenceStore) GetEntry(ctx context.Context,
	canonicalID string) (sqlc.ActivityEntry, error) {

	var entry sqlc.ActivityEntry

	err := s.db.ExecTx(ctx, ReadTxOption(), func(q ActivityStore) error {
		var err error
		entry, err = q.GetActivityEntry(ctx, canonicalID)

		return err
	})

	return entry, err
}

// CountByStatus returns the number of current-state rows in the given status.
// Unlike ListEntries it is not paginated, so it backs the wallet status
// summary's pending count with a true full-feed total.
func (s *ActivityPersistenceStore) CountByStatus(ctx context.Context,
	status int64) (int64, error) {

	var count int64

	err := s.db.ExecTx(ctx, ReadTxOption(), func(q ActivityStore) error {
		var err error
		count, err = q.CountActivityEntriesByStatus(ctx, status)

		return err
	})

	return count, err
}

// ListEntries returns up to limit current-state rows newest-first, starting
// after the (cursorCreated, cursorID) keyset. A cursorCreated of 0 starts from
// the newest row. The cursor is the immutable (created_at_unix, canonical_id)
// of the last row returned.
func (s *ActivityPersistenceStore) ListEntries(ctx context.Context,
	cursorCreated int64, cursorID string, limit int32) (
	[]sqlc.ActivityEntry, error) {

	var rows []sqlc.ActivityEntry

	err := s.db.ExecTx(ctx, ReadTxOption(), func(q ActivityStore) error {
		var err error
		rows, err = q.ListActivityEntries(
			ctx, sqlc.ListActivityEntriesParams{
				CursorCreated: cursorCreated,
				CursorID:      cursorID,
				LimitCount:    limit,
			},
		)

		return err
	})

	return rows, err
}

// ListEntriesByKindStatus returns up to limit entries of the given kind and
// status, paged by canonical_id ascending after cursorID. Filtering in SQL
// keeps a scan for a specific kind/status (e.g. the startup rehydration of
// PENDING EXIT rows) proportional to the matching rows, and the unique
// canonical_id cursor is strictly monotonic.
func (s *ActivityPersistenceStore) ListEntriesByKindStatus(ctx context.Context,
	kind, status int64, cursorID string, limit int32) ([]sqlc.ActivityEntry,
	error) {

	var rows []sqlc.ActivityEntry

	err := s.db.ExecTx(ctx, ReadTxOption(), func(q ActivityStore) error {
		var err error
		rows, err = q.ListEntriesByKindStatus(
			ctx, sqlc.ListEntriesByKindStatusParams{
				Kind:       kind,
				Status:     status,
				CursorID:   cursorID,
				LimitCount: limit,
			},
		)

		return err
	})

	return rows, err
}

// PullEvents returns up to limit transition rows strictly after the event_seq
// cursor, in ascending event_seq order — the resumable-subscribe replay.
func (s *ActivityPersistenceStore) PullEvents(ctx context.Context, cursor int64,
	limit int32) ([]sqlc.ActivityEvent, error) {

	var rows []sqlc.ActivityEvent

	err := s.db.ExecTx(ctx, ReadTxOption(), func(q ActivityStore) error {
		var err error
		rows, err = q.PullActivityEvents(
			ctx, sqlc.PullActivityEventsParams{
				Cursor:     cursor,
				LimitCount: limit,
			},
		)

		return err
	})

	return rows, err
}

// changesRow reports whether the projection would alter the current-state row
// e, i.e. whether it represents a genuine lifecycle transition. It mirrors the
// UpsertActivityEntry semantics: scalar lifecycle columns overwrite directly,
// while an empty note and the settlement/correlation handles preserve their
// stored values. RequestJSON is compared semantically because a later rich
// projection may be the first source of immutable invoice context. EntryJSON
// is not compared because it is the event representation of the effective row,
// not an independently mutable current-state field.
func (p ActivityProjection) changesRow(e sqlc.ActivityEntry) bool {
	if p.PendingStatus != 0 && p.Status == p.PendingStatus &&
		e.Status != p.PendingStatus {
		return false
	}

	if p.Kind != e.Kind ||
		p.Status != e.Status ||
		p.AmountSat != e.AmountSat ||
		p.FeeSat != e.FeeSat ||
		p.Counterparty != e.Counterparty ||
		p.Phase != e.Phase ||
		p.PhaseLabel != e.PhaseLabel ||
		p.FailureCode != e.FailureCode ||
		p.FailureReason != e.FailureReason ||
		p.VtxoOutpoint != e.VtxoOutpoint {
		return true
	}
	if p.Note != "" && p.Note != e.Note {
		return true
	}
	if jsonValueChanges(p.RequestJSON, e.RequestJson) {
		return true
	}

	if blobChanges(p.PaymentHash, e.PaymentHash) ||
		blobChanges(p.Txid, e.Txid) ||
		blobChanges(p.SwapSessionID, e.SwapSessionID) ||
		blobChanges(p.LedgerTxid, e.LedgerTxid) ||
		blobChanges(p.BoardingAddr, e.BoardingAddr) {
		return true
	}

	if p.ConfirmationHeight != nil {
		if !e.ConfirmationHeight.Valid ||
			*p.ConfirmationHeight != e.ConfirmationHeight.Int64 {
			return true
		}
	}

	return false
}

// jsonValueChanges reports whether a non-empty JSON projection changes the
// stored semantic value. Empty input preserves the current value. Malformed
// input falls back to exact comparison so corruption is never hidden as a
// change-suppressed no-op.
func jsonValueChanges(next, stored string) bool {
	if next == "" || next == stored {
		return false
	}

	var nextValue, storedValue any
	if err := json.Unmarshal([]byte(next), &nextValue); err != nil {
		return next != stored
	}
	if err := json.Unmarshal([]byte(stored), &storedValue); err != nil {
		return next != stored
	}

	return !reflect.DeepEqual(nextValue, storedValue)
}

// blobChanges reports whether a COALESCEd blob handle changes the stored value:
// a nil/empty next value preserves the stored one (no change), a non-empty next
// value is a change only when it differs.
func blobChanges(next, stored []byte) bool {
	return len(next) > 0 && !bytes.Equal(next, stored)
}

// nullInt64 converts an optional int64 to sql.NullInt64, mapping nil to the
// NULL sentinel so an unknown confirmation height stays NULL in the DB.
func nullInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: *v, Valid: true}
}
