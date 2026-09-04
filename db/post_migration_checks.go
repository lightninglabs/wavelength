package db

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/ledger"
)

// postMigrationCheck is a function type for a function that performs a
// post-migration check on the database.
type postMigrationCheck func(context.Context, sqlc.Querier) error

var (
	// postMigrationChecks is a map of functions that are run after the
	// database migration with the version specified in the key has been
	// applied. These functions are used to perform additional checks on the
	// database state that are not fully expressible in SQL.
	postMigrationChecks = map[uint]postMigrationCheck{
		// Migration 15 adds the round_uuid TEXT mirror of the ledger's
		// raw round_id BLOB; the string conversion itself is only
		// expressible in Go.
		15: backfillLedgerRoundUUIDs,

		// Migration 19 replaces ambiguous outpoint-only refresh and
		// exit keys with versioned operation-and-leg identities. It
		// also repairs the missing exit send row left by the legacy
		// collision.
		19: reconcileLedgerIdempotencyKeys,
	}
)

const legacyLedgerOutpointKeyLen = 36

type legacyLedgerGroup struct {
	refreshSend *sqlc.LedgerEntry
	exitSend    *sqlc.LedgerEntry
	exitFee     *sqlc.LedgerEntry
}

// reconcileLedgerIdempotencyKeys rewrites legacy refresh and unilateral-exit
// identities into separate versioned domains. When the old shared identity
// suppressed an exit send, the surviving refresh send and exit fee contain
// enough information to reconstruct the missing net-value leg exactly.
func reconcileLedgerIdempotencyKeys(ctx context.Context, q sqlc.Querier) error {
	rows, err := q.ListLegacyOutpointLedgerEntries(ctx)
	if err != nil {
		return fmt.Errorf("list legacy ledger identities: %w", err)
	}

	groups := make(map[string]*legacyLedgerGroup)
	orderedGroups := make([]*legacyLedgerGroup, 0)
	for i := range rows {
		row := rows[i]
		group := groups[string(row.IdempotencyKey)]
		if group == nil {
			group = &legacyLedgerGroup{}
			groups[string(row.IdempotencyKey)] = group
			orderedGroups = append(orderedGroups, group)
		}

		switch {
		case row.EventType == ledger.EventVTXOSent &&
			len(row.RoundID) > 0:

			if group.refreshSend != nil {
				return fmt.Errorf("duplicate legacy refresh " +
					"send identity")
			}
			group.refreshSend = &row

		case row.EventType == ledger.EventVTXOSent:
			if group.exitSend != nil {
				return fmt.Errorf("duplicate legacy exit " +
					"send identity")
			}
			group.exitSend = &row

		case row.EventType == ledger.EventOnchainFeePaid:
			if group.exitFee != nil {
				return fmt.Errorf("duplicate legacy exit fee " +
					"identity")
			}
			group.exitFee = &row
		}
	}

	for _, group := range orderedGroups {
		for _, row := range []*sqlc.LedgerEntry{
			group.refreshSend, group.exitSend, group.exitFee,
		} {
			if row == nil {
				continue
			}

			hash, index, err := decodeLegacyLedgerOutpoint(
				row.IdempotencyKey,
			)
			if err != nil {
				return err
			}

			var newKey []byte
			switch {
			case row == group.refreshSend:
				newKey = ledger.RefreshSendIdempotencyKey(
					hash, index,
				)

			case row == group.exitSend:
				newKey = ledger.ExitSendIdempotencyKey(
					hash, index,
				)

			case row == group.exitFee:
				newKey = ledger.ExitFeeIdempotencyKey(
					hash, index,
				)
			}

			updated, err := rewriteLegacyLedgerIdentity(
				ctx, q, row, newKey, hash, index,
				row == group.refreshSend,
			)
			if err != nil {
				return fmt.Errorf("rewrite ledger identity "+
					"%d: %w", row.EntryID, err)
			}
			if updated != 1 {
				return fmt.Errorf("rewrite ledger identity "+
					"%d: updated %d rows", row.EntryID,
					updated)
			}
		}

		if group.refreshSend == nil || group.exitFee == nil ||
			group.exitSend != nil {

			continue
		}

		if group.exitFee.AmountSat >= group.refreshSend.AmountSat {
			return fmt.Errorf("legacy exit fee %d is not below "+
				"VTXO amount %d", group.exitFee.AmountSat,
				group.refreshSend.AmountSat)
		}

		hash, index, err := decodeLegacyLedgerOutpoint(
			group.exitFee.IdempotencyKey,
		)
		if err != nil {
			return err
		}

		const feeDescriptionPrefix = "exit cost for "
		if !strings.HasPrefix(
			group.exitFee.Description, feeDescriptionPrefix,
		) {
			return fmt.Errorf("legacy exit fee %d has unknown "+
				"description", group.exitFee.EntryID)
		}
		description := "unilateral exit net value for " +
			strings.TrimPrefix(
				group.exitFee.Description, feeDescriptionPrefix,
			)

		chainVout := int32(index)
		err = q.InsertClientLedgerEntry(
			ctx, sqlc.InsertClientLedgerEntryParams{
				DebitAccount:  ledger.AccountTransfersOut,
				CreditAccount: ledger.AccountVTXOBalance,
				AmountSat: group.refreshSend.AmountSat -
					group.exitFee.AmountSat,
				IdempotencyKey: ledger.ExitSendIdempotencyKey(
					hash, index,
				),
				EventType:   ledger.EventVTXOSent,
				Description: description,
				CreatedAt:   group.exitFee.CreatedAt,
				ChainTxid:   hash[:],
				ChainVout:   sqlInt32Ptr(&chainVout),
			},
		)
		if err != nil {
			return fmt.Errorf("repair missing exit send: %w", err)
		}
	}

	return nil
}

// rewriteLegacyLedgerIdentity applies the namespaced key and adds structured
// outpoint metadata to unilateral-exit rows. Refresh sends keep their existing
// chain metadata because their paired receive already identifies the output.
func rewriteLegacyLedgerIdentity(ctx context.Context, q sqlc.Querier,
	row *sqlc.LedgerEntry, newKey []byte, hash [32]byte, index uint32,
	refresh bool) (int64, error) {

	if refresh {
		return q.UpdateLedgerEntryIdempotencyKey(
			ctx, sqlc.UpdateLedgerEntryIdempotencyKeyParams{
				NewKey:  newKey,
				EntryID: row.EntryID,
				OldKey:  row.IdempotencyKey,
			},
		)
	}

	chainVout := int32(index)

	return q.UpdateLegacyExitLedgerEntry(
		ctx, sqlc.UpdateLegacyExitLedgerEntryParams{
			NewKey:    newKey,
			ChainTxid: hash[:],
			ChainVout: sqlInt32Ptr(&chainVout),
			EntryID:   row.EntryID,
			OldKey:    row.IdempotencyKey,
		},
	)
}

// decodeLegacyLedgerOutpoint decodes the old 32-byte hash plus big-endian
// uint32 output-index identity.
func decodeLegacyLedgerOutpoint(key []byte) ([32]byte, uint32, error) {
	var hash [32]byte
	if len(key) != legacyLedgerOutpointKeyLen {
		return hash, 0, fmt.Errorf("legacy ledger key has length %d",
			len(key))
	}

	copy(hash[:], key[:32])
	index := binary.BigEndian.Uint32(key[32:])

	return hash, index, nil
}

// backfillLedgerRoundUUIDs mirrors every distinct raw 16-byte round_id in
// ledger_entries into the round_uuid TEXT column added by migration 15, using
// the same canonical lowercase form that rounds.round_id and
// vtxos.forfeit_round_id store. Rows whose round_id is not exactly 16 bytes
// (which no writer produces) are left NULL rather than failing the whole
// migration. The per-round UPDATE is guarded on round_uuid IS NULL, so a
// crash-interrupted backfill re-runs as a no-op for already-converted rows.
func backfillLedgerRoundUUIDs(ctx context.Context, q sqlc.Querier) error {
	roundIDs, err := q.ListLedgerRoundIDsMissingUuid(ctx)
	if err != nil {
		return fmt.Errorf("list ledger round ids missing uuid: %w", err)
	}

	for _, rawID := range roundIDs {
		// Abort early on a cancelled migration context rather than
		// issuing further per-round writes.
		if err := ctx.Err(); err != nil {
			return err
		}

		if len(rawID) != 16 {
			continue
		}

		var id uuid.UUID
		copy(id[:], rawID)

		err := q.BackfillLedgerRoundUuid(
			ctx, sqlc.BackfillLedgerRoundUuidParams{
				RoundUuid: sql.NullString{
					String: id.String(),
					Valid:  true,
				},
				RoundID: rawID,
			},
		)
		if err != nil {
			return fmt.Errorf("backfill ledger round uuid %s: %w",
				id, err)
		}
	}

	return nil
}

// DatabaseBackend is an interface that contains all methods our different
// database backends implement.
type DatabaseBackend interface {
	BatchedQuerier

	WithTx(tx *sql.Tx) *sqlc.Queries
}

// makePostStepCallbacks turns the post migration checks into a map of post
// step callbacks that can be used with the migrate package. The keys of the map
// are the migration versions, and the values are the callbacks that will be
// executed after the migration with the corresponding version is applied.
func makePostStepCallbacks(db DatabaseBackend, log btclog.Logger,
	checks map[uint]postMigrationCheck) map[uint]migrate.PostStepCallback {

	var (
		ctx  = context.Background()
		txDB = NewTransactionExecutor(
			db, func(tx *sql.Tx) sqlc.Querier {
				return db.WithTx(tx)
			}, log,
		)
		writeTxOpts = WriteTxOption()
	)

	postStepCallbacks := make(map[uint]migrate.PostStepCallback)
	for version, check := range checks {
		// Capture the check in a closure.
		checkFn := check

		runCheck := func(m *migrate.Migration, q sqlc.Querier) error {
			log.InfoS(ctx, "Running post-migration check",
				"version", version,
			)
			start := time.Now()

			err := checkFn(ctx, q)
			if err != nil {
				return fmt.Errorf("post-migration check "+
					"failed for version %d: %w", version,
					err)
			}

			log.InfoS(ctx, "Post-migration check completed",
				"version", version,
				"duration", time.Since(start),
			)

			return nil
		}

		// We ignore the actual driver that's being returned here, since
		// we use migrate.NewWithInstance() to create the migration
		// instance from our already instantiated database backend that
		// is also passed into this function.
		postStepCallbacks[version] = func(m *migrate.Migration,
			_ database.Driver) error {

			return txDB.ExecTx(
				ctx, writeTxOpts, func(q sqlc.Querier) error {
					return runCheck(m, q)
				},
			)
		}
	}

	return postStepCallbacks
}
