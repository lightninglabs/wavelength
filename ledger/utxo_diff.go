package ledger

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
)

// WalletUTXO is the minimal description of an unspent output in
// the operator's treasury wallet that the UTXO diff subsystem
// consumes. Matches the shape expected to be produced by
// lndbackend's wrapper around lnd's WalletKit.ListUnspent — the
// concrete wiring lands in a follow-up PR.
type WalletUTXO struct {
	// Outpoint identifies the UTXO on chain.
	Outpoint wire.OutPoint

	// Amount is the UTXO value.
	Amount btcutil.Amount
}

// WalletUTXOLister is the interface the ledger actor uses to
// fetch the current treasury wallet UTXO set at each block
// epoch. Configured via ActorConfig.WalletUTXOLister; when
// None, the diff subsystem is inert and handleBlockEpoch
// degrades to a log-only no-op. This keeps the actor useful
// on deployments that have not yet wired the wallet side.
type WalletUTXOLister interface {
	// ListUnspent returns every UTXO currently controlled by
	// the operator's treasury wallet. The set does not need
	// to be sorted; the diff subsystem builds its own
	// outpoint-keyed maps.
	ListUnspent(ctx context.Context) ([]WalletUTXO, error)
}

// UTXOAuditEvent is the canonical enum for the wallet_utxo_log
// table's event column. "created" and "spent" mirror the enum
// rows seeded by migration 000011_utxo_audit_log.
type UTXOAuditEvent string

const (
	// UTXOAuditCreated records a wallet UTXO appearing in the
	// snapshot for the first time.
	UTXOAuditCreated UTXOAuditEvent = "created"

	// UTXOAuditSpent records a wallet UTXO that was present
	// in the previous snapshot but is no longer in the
	// current one.
	UTXOAuditSpent UTXOAuditEvent = "spent"
)

// UTXOClassification is the enum for the wallet_utxo_log
// classified_as column — the diff subsystem's best guess at
// why a UTXO appeared or disappeared. Matches the rows seeded
// by migration 000011_utxo_audit_log.
type UTXOClassification string

const (
	// UTXOClassDeposit labels a created UTXO whose origin is
	// not attributable to a round or sweep. Business users
	// read this as "operator topped up the wallet".
	UTXOClassDeposit UTXOClassification = "deposit"

	// UTXOClassSweepReturn labels a created UTXO whose origin
	// is an expired-VTXO sweep transaction. Capital returning
	// from deployed_capital to the treasury wallet.
	UTXOClassSweepReturn UTXOClassification = "sweep_return"

	// UTXOClassRoundFunding labels a spent UTXO that funded a
	// round commitment transaction.
	UTXOClassRoundFunding UTXOClassification = "round_funding"

	// UTXOClassChange labels a round-change UTXO that came
	// back to the treasury wallet.
	UTXOClassChange UTXOClassification = "change"

	// UTXOClassUnknown labels a movement the classifier
	// cannot attribute. In the initial implementation every
	// unknown-origin event is booked as external (deposit or
	// withdrawal) on the ledger side — when round / sweep
	// tracking lands, unknown should trend toward zero.
	UTXOClassUnknown UTXOClassification = "unknown"
)

// WalletUTXOLogEntry is the domain-level representation of a
// single wallet_utxo_log row. The `ledger.UTXOAuditStore`
// interface takes this shape; the db adapter flattens typed
// enum values and time.Time into the sqlc params.
type WalletUTXOLogEntry struct {
	// Outpoint uniquely identifies the UTXO. The UNIQUE
	// constraint on (hash, index, event) plus ON CONFLICT DO
	// NOTHING at the sqlc layer makes the insert replay-safe.
	Outpoint wire.OutPoint

	// Amount is the UTXO value.
	Amount btcutil.Amount

	// Event is "created" or "spent".
	Event UTXOAuditEvent

	// BlockHeight is the block at which the diff detected
	// this change.
	BlockHeight int64

	// Classification is the diff subsystem's best guess at
	// the economic origin of the movement.
	Classification UTXOClassification

	// CreatedAt is the wall-clock time at which this row was
	// produced.
	CreatedAt time.Time
}

// UTXOAuditStore is the write-only interface the ledger actor
// uses to persist wallet_utxo_log rows. Configured via
// ActorConfig.UTXOAuditStore; when None, audit writes are
// skipped and the diff subsystem still runs (writing the
// ledger legs when applicable).
type UTXOAuditStore interface {
	// InsertWalletUTXOLog persists a single audit row. The
	// implementation is expected to be idempotent via ON
	// CONFLICT DO NOTHING against UNIQUE(hash, index, event).
	InsertWalletUTXOLog(
		ctx context.Context, entry WalletUTXOLogEntry,
	) error
}

// UTXOSnapshotReader reconstructs the treasury wallet's current
// UTXO set from the persisted audit log so the ledger actor can
// rehydrate its in-memory diff snapshot across a restart.
// Without this step, a restart silently re-enters seeding mode:
// the first post-restart block epoch writes audit rows but
// skips the external_deposit / external_withdrawal ledger legs,
// permanently losing attribution for any on-chain movement that
// happened while the actor was down.
//
// Decoupled from UTXOAuditStore (write-only) so the write path
// does not widen its surface. The same db adapter satisfies
// both interfaces in production; tests can supply either a
// combined or a split mock.
type UTXOSnapshotReader interface {
	// ListLiveWalletUTXOs returns every outpoint that has a
	// 'created' audit row without a paired 'spent' row --
	// i.e. the current live UTXO set reconstructed from the
	// append-only log. The second return value is the highest
	// block_height observed among live rows, used to bootstrap
	// the tracker's last-seen height.
	ListLiveWalletUTXOs(
		ctx context.Context,
	) ([]WalletUTXO, int64, error)

	// CountAuditRows returns the total number of rows in the
	// wallet_utxo_log table. Used by reseedUTXOSnapshot to
	// distinguish a true fresh install (no rows ever written,
	// seeding pass is legitimate) from a running deployment
	// whose wallet happens to be empty right now (history
	// exists, must NOT re-enter seeding or external movements
	// would be silently dropped on the first post-restart
	// block).
	CountAuditRows(ctx context.Context) (int64, error)
}

// utxoSnapshot is the in-memory mirror of the previous block's
// wallet UTXO set, keyed by outpoint for O(1) diff lookups.
type utxoSnapshot map[wire.OutPoint]btcutil.Amount

// utxoTracker holds the actor's running snapshot of the
// treasury wallet UTXO set. The first handleBlockEpoch after
// startup seeds `prev` from the wallet; subsequent calls diff
// against `prev` and then overwrite it.
//
// No mutex: every access runs on the durable actor's
// single-consumer receive loop (the top-of-CLAUDE.md
// "all accounting writes serialize through one actor"
// invariant), and reseedUTXOSnapshot runs inside Start before
// the mailbox opens. If a diagnostics read-path is ever added,
// introduce a Snapshot() accessor that copies under a
// newly-added lock rather than re-adding lock discipline across
// the whole struct.
type utxoTracker struct {
	prev utxoSnapshot

	// seeded is set after the first snapshot is installed.
	// Until that happens, the actor cannot legitimately call
	// anything "new" or "spent" — a UTXO present on startup
	// might have appeared in any prior block. The seeding
	// pass therefore records wallet_utxo_log rows but does
	// not write external_deposit ledger entries.
	seeded bool
}

// newUTXOTracker returns an empty tracker. The first block
// epoch after Start seeds the snapshot.
func newUTXOTracker() *utxoTracker {
	return &utxoTracker{
		prev: make(utxoSnapshot),
	}
}

// outpointKey returns the 36-byte dedup key used for
// external_deposit / external_withdrawal ledger entries. The
// format (hash || little-endian uint32 index) is stable across
// restarts and matches the client's exitIdempotencyKey shape so
// the two sides use the same layout.
func outpointKey(op wire.OutPoint) []byte {
	buf := make([]byte, 32+4)
	copy(buf, op.Hash[:])
	binary.LittleEndian.PutUint32(buf[32:], op.Index)

	return buf
}

// diffResult captures the set of UTXOs that appeared (created)
// and disappeared (spent) between two snapshots. Returned by
// diffSnapshots so handleBlockEpoch can iterate once without
// juggling inline state.
type diffResult struct {
	created []WalletUTXO
	spent   []WalletUTXO
}

// diffSnapshots computes the symmetric difference between the
// previous and current UTXO sets. Created = in current, not in
// previous; spent = in previous, not in current. Amounts are
// preserved so audit rows and external_* ledger legs book the
// correct value.
func diffSnapshots(prev utxoSnapshot,
	current []WalletUTXO) diffResult {

	// Build a current-side lookup map once; the two passes
	// below each do one membership check per UTXO.
	curr := make(utxoSnapshot, len(current))
	for _, u := range current {
		curr[u.Outpoint] = u.Amount
	}

	var out diffResult

	// Created: in curr, not in prev.
	for op, amt := range curr {
		if _, ok := prev[op]; ok {
			continue
		}
		out.created = append(out.created, WalletUTXO{
			Outpoint: op,
			Amount:   amt,
		})
	}

	// Spent: in prev, not in curr.
	for op, amt := range prev {
		if _, ok := curr[op]; ok {
			continue
		}
		out.spent = append(out.spent, WalletUTXO{
			Outpoint: op,
			Amount:   amt,
		})
	}

	return out
}

// applyUTXODiff writes audit rows for each created / spent
// entry. The subsystem is intentionally audit-only: every
// treasury_wallet movement the round actor or batch sweeper
// produces is already booked by those actors (RecordCapital-
// Committed, RecordRoundSweep, RecordMiningFee), so booking
// external_deposit / external_withdrawal here would double-
// count round-change and round-funding UTXOs on every block.
// The UTXO diff loop will regain ledger-booking authority once
// the classifier lands — see the classifier PR tracking issue
// referenced in ledger/CLAUDE.md. Until then, audit rows are a
// pure observability signal that lets operators see every
// wallet-level movement without affecting ledger totals.
func (a *LedgerActor) applyUTXODiff(ctx context.Context,
	diff diffResult, blockHeight int64) error {

	now := a.clk.Now()

	// Created UTXOs: audit row only. Classification stays as
	// UTXOClassDeposit for backward compatibility with the
	// existing schema enum; the classifier PR promotes it to
	// the real class (round_change / sweep_return / deposit).
	for _, u := range diff.created {
		if err := a.writeAuditRow(ctx, WalletUTXOLogEntry{
			Outpoint:       u.Outpoint,
			Amount:         u.Amount,
			Event:          UTXOAuditCreated,
			BlockHeight:    blockHeight,
			Classification: UTXOClassDeposit,
			CreatedAt:      now,
		}); err != nil {
			return err
		}
	}

	// Spent UTXOs: audit row only. Classified as Unknown
	// until the classifier lands.
	for _, u := range diff.spent {
		if err := a.writeAuditRow(ctx, WalletUTXOLogEntry{
			Outpoint:       u.Outpoint,
			Amount:         u.Amount,
			Event:          UTXOAuditSpent,
			BlockHeight:    blockHeight,
			Classification: UTXOClassUnknown,
			CreatedAt:      now,
		}); err != nil {
			return err
		}
	}

	return nil
}

// writeAuditRow persists one wallet_utxo_log row if an audit
// store is configured. A nil store is not an error: the diff
// subsystem still runs its in-memory snapshot tracking and the
// audit trail is simply disabled.
func (a *LedgerActor) writeAuditRow(ctx context.Context,
	entry WalletUTXOLogEntry) error {

	if !a.cfg.UTXOAuditStore.IsSome() {
		return nil
	}
	store := a.cfg.UTXOAuditStore.UnsafeFromSome()

	if err := store.InsertWalletUTXOLog(ctx, entry); err != nil {
		return fmt.Errorf(
			"insert wallet_utxo_log: %w", err,
		)
	}

	return nil
}

// reseedUTXOSnapshot rehydrates the actor's in-memory UTXO
// diff snapshot from the persisted audit log before the mailbox
// begins accepting messages. Without this step, any UTXO spent
// during daemon downtime is silently missed in the audit log:
// an empty prev snapshot + post-downtime current set makes the
// diff see only "created" entries for what remained, and the
// disappeared outpoints never produce a "spent" audit row.
// (Created-side duplicates are harmless — the UNIQUE(hash,
// index, event) constraint + ON CONFLICT DO NOTHING drops them.
// It's the spent side that depends on a correctly rehydrated
// prev.)
//
// No-op when either the WalletUTXOLister is absent (the diff
// subsystem is inert, nothing to seed) or the snapshot reader
// is absent (unit-test harness without a wired db adapter; the
// tracker stays at its zero state and the first block epoch
// performs a fresh seeding pass).
//
// An empty live set is NOT sufficient evidence of a fresh
// install: a long-running deployment can end up with zero live
// UTXOs (everything swept, pending boarding, etc.) while the
// audit log holds thousands of historical rows. Re-entering
// seeding in that state would still be correct for ledger
// totals (the diff is audit-only), but the `seeded` flag is
// used by tests and operator logs as a liveness signal, so we
// keep the fresh-install vs empty-but-historical distinction.
func (a *LedgerActor) reseedUTXOSnapshot(ctx context.Context) error {
	if !a.cfg.WalletUTXOLister.IsSome() {
		return nil
	}
	if !a.cfg.UTXOSnapshotReader.IsSome() {
		return nil
	}

	reader := a.cfg.UTXOSnapshotReader.UnsafeFromSome()

	live, maxBlockHeight, err := reader.ListLiveWalletUTXOs(ctx)
	if err != nil {
		return fmt.Errorf("list live UTXOs: %w", err)
	}

	if len(live) == 0 {
		// Distinguish a true fresh install from a running
		// deployment whose wallet is empty at this instant.
		// A row count > 0 means the audit log has prior
		// history even though no outpoints are currently
		// live, so the next created UTXO is genuinely new
		// and must be booked as external_deposit -- not
		// folded into a seeding pass.
		rowCount, err := reader.CountAuditRows(ctx)
		if err != nil {
			return fmt.Errorf("count audit rows: %w", err)
		}

		if rowCount == 0 {
			a.log.InfoS(ctx,
				"UTXO snapshot reseed found empty "+
					"audit log, first block epoch "+
					"will seed baseline",
			)

			return nil
		}

		a.utxo.prev = make(utxoSnapshot)
		a.utxo.seeded = true

		a.log.InfoS(ctx,
			"UTXO snapshot reseeded empty with history",
			slog.Int64("audit_row_count", rowCount),
		)

		return nil
	}

	snapshot := make(utxoSnapshot, len(live))
	for _, u := range live {
		snapshot[u.Outpoint] = u.Amount
	}

	a.utxo.prev = snapshot
	a.utxo.seeded = true

	a.log.InfoS(ctx, "UTXO snapshot reseeded from audit log",
		slog.Int("utxo_count", len(live)),
		slog.Int64("last_block_height", maxBlockHeight),
	)

	return nil
}

// processBlockUTXODiff is the entry point invoked from
// handleBlockEpoch when the WalletUTXOLister is configured. It
// acquires the wallet's current UTXO set, computes the diff
// against the actor's previous snapshot, writes audit rows,
// then replaces the snapshot. Ledger legs are NOT booked
// here -- see applyUTXODiff for the audit-only rationale.
func (a *LedgerActor) processBlockUTXODiff(ctx context.Context,
	blockHeight int64) error {

	if !a.cfg.WalletUTXOLister.IsSome() {
		return nil
	}
	lister := a.cfg.WalletUTXOLister.UnsafeFromSome()

	current, err := lister.ListUnspent(ctx)
	if err != nil {
		return fmt.Errorf("list wallet UTXOs: %w", err)
	}

	diff := diffSnapshots(a.utxo.prev, current)

	a.log.InfoS(ctx, "Wallet UTXO diff",
		slog.Int64("block_height", blockHeight),
		slog.Int("created", len(diff.created)),
		slog.Int("spent", len(diff.spent)),
		slog.Bool("seeded", a.utxo.seeded),
	)

	if err := a.applyUTXODiff(ctx, diff, blockHeight); err != nil {
		return err
	}

	// Replace the snapshot only after all writes succeed so
	// a transient error leaves the prev snapshot intact and
	// the next epoch retries naturally.
	next := make(utxoSnapshot, len(current))
	for _, u := range current {
		next[u.Outpoint] = u.Amount
	}
	a.utxo.prev = next
	a.utxo.seeded = true

	return nil
}
