package db

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

var (
	// ErrCreditOperationNotFound indicates the credit operation row does
	// not exist.
	ErrCreditOperationNotFound = errors.New("credit operation row not " +
		"found")
)

// CreditOpKind records the credit operation family. Values are append-only;
// the numeric meaning of an existing value must never shift.
type CreditOpKind int32

const (
	// CreditOpKindPay is a sub-dust / shortfall pay (optional Ark top-up,
	// then credit or mixed pay).
	CreditOpKindPay CreditOpKind = iota + 1

	// CreditOpKindReceive is a server-owned Lightning receive that credits
	// the account.
	CreditOpKindReceive

	// CreditOpKindRedeem materializes available credits back into an Ark
	// vTXO.
	CreditOpKindRedeem
)

// CreditOpStatus is the coordinator-facing status of one credit operation.
// Values are append-only.
type CreditOpStatus int32

const (
	// CreditOpStatusPending means the operation is still in flight.
	CreditOpStatusPending CreditOpStatus = iota

	// CreditOpStatusCompleted means the operation completed successfully.
	CreditOpStatusCompleted

	// CreditOpStatusFailed means the operation failed terminally.
	CreditOpStatusFailed
)

// IsTerminal reports whether the operation status is terminal.
func (s CreditOpStatus) IsTerminal() bool {
	return s == CreditOpStatusCompleted || s == CreditOpStatusFailed
}

// CreditOperationRecord is one credit operation's full durable state: the
// queryable control-plane fields plus the opaque resume snapshot. It is the
// single source of truth for the client's progress through one credit flow --
// the per-operation actor reads and writes it directly inside its
// Read/Stage/Commit phases rather than using the generic actor-delivery
// fsm_checkpoints blob. The server credit ledger remains authoritative for the
// money.
type CreditOperationRecord struct {
	// OpID is the stable, unique per-admission credit operation identifier.
	OpID string

	// OpKey is the stable client idempotency key (pay:<hash> / recv:<hash>
	// / redeem:<id>) reused for the server op AND the delegated OOR
	// transfer.
	OpKey string

	// Kind records the credit operation family.
	Kind CreditOpKind

	// State is the latest FSM state string.
	State string

	// Status is the coordinator-facing operation status.
	Status CreditOpStatus

	// ServerOpID is the swap-server credit operation id, when known.
	ServerOpID string

	// PaymentHash is the BOLT-11 payment hash for pay and receive ops.
	PaymentHash []byte

	// DestinationPubkey is the server-owned Ark destination for a top-up,
	// or the wallet-owned receive destination for a redemption.
	DestinationPubkey []byte

	// OORSessionID is the delegated OOR transfer session id, when admitted.
	OORSessionID string

	// Invoice is the target invoice (pay) or server receive invoice (recv).
	Invoice string

	// AmountSat is the principal amount for the operation.
	AmountSat int64

	// TopupSat is the Ark top-up amount required to cover a pay shortfall.
	TopupSat int64

	// MaxCreditSat is the credit cap passed to StartPay for a pay op.
	MaxCreditSat int64

	// MaxFeeSat is the caller's max routing fee for a pay op.
	MaxFeeSat int64

	// RoutingFeeBudgetSat is the client-funded Lightning routing allowance.
	RoutingFeeBudgetSat int64

	// LastError is the latest terminal failure reason.
	LastError string

	// SnapshotData is the TLV-encoded per-operation resume snapshot.
	SnapshotData []byte

	// SnapshotVersion is the encoding version of SnapshotData.
	SnapshotVersion int32

	// CreatedAt is when the row was first written.
	CreatedAt time.Time

	// UpdatedAt is when the row was last updated.
	UpdatedAt time.Time
}

// CreditOperationStoreDB bridges the credit operation control-plane to the
// sqlc-generated queries. Every method wraps the query in ExecTx so that, when
// ctx carries a durable-actor transaction (actor.TxFromContext), the write
// joins that outer tx and commits atomically alongside the mailbox ack; from
// the registry actor (no ambient tx) it opens its own short transaction.
type CreditOperationStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]

	clock clock.Clock
}

// NewCreditOperationStore creates a credit operation store from a Store.
func NewCreditOperationStore(store *Store,
	clk clock.Clock) *CreditOperationStoreDB {

	baseDB := store.BaseDB()

	txExec := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return store.Queries().WithTx(tx)
		},
		store.log,
	)

	return &CreditOperationStoreDB{
		TransactionExecutor: txExec,
		clock:               clk,
	}
}

// UpsertOperation persists or updates one credit operation row.
func (s *CreditOperationStoreDB) UpsertOperation(ctx context.Context,
	record CreditOperationRecord) error {

	nowUnix := s.clock.Now().Unix()
	createdAt := record.CreatedAt.Unix()
	if record.CreatedAt.IsZero() {
		createdAt = nowUnix
	}

	return s.ExecTx(
		ctx, WriteTxOption(),
		func(q *sqlc.Queries) error {
			return q.UpsertCreditOperation(
				ctx,
				sqlc.UpsertCreditOperationParams{
					OpID:  record.OpID,
					OpKey: record.OpKey,
					Kind:  int32(record.Kind),
					State: record.State,
					Status: int32(
						record.Status,
					),
					ServerOpID: nullString(
						record.ServerOpID,
					),
					PaymentHash: record.PaymentHash,
					DestinationPubkey: record.
						DestinationPubkey,
					OorSessionID: nullString(
						record.OORSessionID,
					),
					Invoice: nullString(
						record.Invoice,
					),
					AmountSat:    record.AmountSat,
					TopupSat:     record.TopupSat,
					MaxCreditSat: record.MaxCreditSat,
					MaxFeeSat:    record.MaxFeeSat,
					RoutingFeeBudgetSat: record.
						RoutingFeeBudgetSat,
					LastError: nullString(
						record.LastError,
					),
					SnapshotData:    record.SnapshotData,
					SnapshotVersion: record.SnapshotVersion,
					CreatedAt:       createdAt,
					UpdatedAt:       nowUnix,
				},
			)
		},
	)
}

// GetOperation loads one credit operation row by op id.
func (s *CreditOperationStoreDB) GetOperation(ctx context.Context,
	opID string) (*CreditOperationRecord, error) {

	var record *CreditOperationRecord

	readFn := func(q *sqlc.Queries) error {
		row, err := q.GetCreditOperation(ctx, opID)
		if err != nil {
			return err
		}

		converted := creditOperationRecordFromRow(row)
		record = &converted

		return nil
	}

	err := s.ExecTx(ctx, ReadTxOption(), readFn)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCreditOperationNotFound
	}
	if err != nil {
		return nil, err
	}

	return record, nil
}

// LookupActiveOperationByKey loads the non-failed credit operation row carrying
// the given op_key, if any. Failed operations are excluded so a keyed retry
// after a failure admits a fresh operation instead of deduping against the dead
// one; pending and completed operations still answer for the key.
func (s *CreditOperationStoreDB) LookupActiveOperationByKey(ctx context.Context,
	key string) (*CreditOperationRecord, error) {

	if key == "" {
		return nil, ErrCreditOperationNotFound
	}

	var record *CreditOperationRecord

	readFn := func(q *sqlc.Queries) error {
		row, err := q.LookupActiveCreditOperationByKey(ctx, key)
		if err != nil {
			return err
		}

		converted := creditOperationRecordFromRow(row)
		record = &converted

		return nil
	}

	err := s.ExecTx(ctx, ReadTxOption(), readFn)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCreditOperationNotFound
	}
	if err != nil {
		return nil, err
	}

	return record, nil
}

// ListNonTerminal loads every non-terminal credit operation row. The registry
// actor uses this on boot to respawn in-flight per-operation actors.
func (s *CreditOperationStoreDB) ListNonTerminal(ctx context.Context) (
	[]CreditOperationRecord, error) {

	result := make([]CreditOperationRecord, 0)

	readFn := func(q *sqlc.Queries) error {
		rows, err := q.ListNonTerminalCreditOperations(ctx)
		if err != nil {
			return err
		}

		result = make([]CreditOperationRecord, 0, len(rows))
		for i := range rows {
			result = append(
				result, creditOperationRecordFromRow(rows[i]),
			)
		}

		return nil
	}

	if err := s.ExecTx(ctx, ReadTxOption(), readFn); err != nil {
		return nil, err
	}

	return result, nil
}

// ListOperations returns every credit operation row, terminal and
// non-terminal alike, for coarse diagnostic listings.
func (s *CreditOperationStoreDB) ListOperations(ctx context.Context) (
	[]CreditOperationRecord, error) {

	result := make([]CreditOperationRecord, 0)

	readFn := func(q *sqlc.Queries) error {
		rows, err := q.ListAllCreditOperations(ctx)
		if err != nil {
			return err
		}

		result = make([]CreditOperationRecord, 0, len(rows))
		for i := range rows {
			result = append(
				result, creditOperationRecordFromRow(rows[i]),
			)
		}

		return nil
	}

	if err := s.ExecTx(ctx, ReadTxOption(), readFn); err != nil {
		return nil, err
	}

	return result, nil
}

// creditOperationRecordFromRow converts a sqlc row into a domain record.
func creditOperationRecordFromRow(
	row sqlc.CreditOperation) CreditOperationRecord {

	record := CreditOperationRecord{
		OpID:                row.OpID,
		OpKey:               row.OpKey,
		Kind:                CreditOpKind(row.Kind),
		State:               row.State,
		Status:              CreditOpStatus(row.Status),
		PaymentHash:         row.PaymentHash,
		DestinationPubkey:   row.DestinationPubkey,
		AmountSat:           row.AmountSat,
		TopupSat:            row.TopupSat,
		MaxCreditSat:        row.MaxCreditSat,
		MaxFeeSat:           row.MaxFeeSat,
		RoutingFeeBudgetSat: row.RoutingFeeBudgetSat,
		SnapshotData:        row.SnapshotData,
		SnapshotVersion:     row.SnapshotVersion,
		CreatedAt:           time.Unix(row.CreatedAt, 0),
		UpdatedAt:           time.Unix(row.UpdatedAt, 0),
	}

	if row.ServerOpID.Valid {
		record.ServerOpID = row.ServerOpID.String
	}
	if row.OorSessionID.Valid {
		record.OORSessionID = row.OorSessionID.String
	}
	if row.Invoice.Valid {
		record.Invoice = row.Invoice.String
	}
	if row.LastError.Valid {
		record.LastError = row.LastError.String
	}

	return record
}

// nullString maps a Go string to a sql.NullString that is invalid when empty.
func nullString(s string) sql.NullString {
	return sql.NullString{
		String: s,
		Valid:  s != "",
	}
}
