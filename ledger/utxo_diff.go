package ledger

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/fees"
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

	// UTXOClassRoundChange labels a round-change UTXO. Alias
	// for UTXOClassChange kept so the four handler-emitted
	// classifications mirror each other verbally
	// (round_funding / round_change, sweep_consumption /
	// sweep_return).
	UTXOClassRoundChange UTXOClassification = "round_change"

	// UTXOClassSweepConsumption labels a spent UTXO that was
	// consumed as an input to a batch sweep transaction. The
	// spent-side analogue of UTXOClassSweepReturn.
	UTXOClassSweepConsumption UTXOClassification = "sweep_consumption"

	// UTXOClassWithdrawal labels a spent UTXO the classifier
	// could not attribute to a round or sweep. Booked by the
	// diff loop as RecordExternalWithdrawal. Spent-side
	// analogue of UTXOClassDeposit.
	UTXOClassWithdrawal UTXOClassification = "withdrawal"

	// UTXOClassPending labels an outpoint the diff loop
	// observed on a block epoch but whose matching round /
	// sweep RoundConfirmedMsg or SweepCompletedMsg had not
	// been drained from the mailbox yet. The reconciliation
	// pass at the next block epoch promotes still-pending
	// rows to deposit / withdrawal and books the matching
	// external_* ledger leg.
	UTXOClassPending UTXOClassification = "pending"

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

	// SourceID is the 16-byte round_id or batch_id that
	// attributes the outpoint to a specific round / sweep
	// event. Nil for rows the diff loop produced itself.
	// Handler pre-inserts set this to the producing round /
	// batch ID so operator-side reconciliation can join audit
	// rows back to their source event.
	SourceID []byte
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
	// The rowcount lets the diff loop distinguish a freshly
	// inserted (genuinely external) row from a silent no-op
	// that hit an already-attributed row pre-inserted by the
	// round / sweep handler.
	InsertWalletUTXOLog(
		ctx context.Context, entry WalletUTXOLogEntry,
	) (int64, error)

	// PromotePendingWalletUTXOLog flips every 'pending' audit
	// row whose block_height is strictly below the watermark
	// into its terminal classification ('deposit' for created,
	// 'withdrawal' for spent) and returns the promoted rows.
	// The classifier books the matching external_* ledger leg
	// for each returned row so in-limbo outpoints get
	// reconciled exactly once.
	PromotePendingWalletUTXOLog(
		ctx context.Context, watermark int64,
	) ([]WalletUTXOLogEntry, error)
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
// outpoint observed on a block epoch and delegates external_*
// ledger booking to the grace-window reconciliation pass that
// runs BEFORE this diff (see reconcilePendingAuditRows).
//
// Each new row is inserted with classified_as='pending'. When
// a round / sweep handler has already pre-inserted an
// attributed row for the same (outpoint, event), the UNIQUE
// (hash, index, event) constraint makes this insert a no-op
// and the rowcount comes back zero -- the diff loop then
// knows the outpoint was already attributed and skips it.
// Rows that land as 'pending' stay that way until the next
// block epoch's reconciliation pass promotes them to
// 'deposit' / 'withdrawal' and books the matching external_*
// leg. A one-block grace window lets the handler's
// RoundConfirmedMsg / SweepCompletedMsg drain from the
// mailbox after the BlockEpochMsg that carried the
// confirmation, covering the narrow race where the two
// messages arrive out of order on the ledger actor's
// single-consumer receive loop.
func (a *LedgerActor) applyUTXODiff(ctx context.Context,
	diff diffResult, blockHeight int64) error {

	now := a.clk.Now()

	for _, u := range diff.created {
		if _, err := a.writeAuditRow(
			ctx, WalletUTXOLogEntry{
				Outpoint:       u.Outpoint,
				Amount:         u.Amount,
				Event:          UTXOAuditCreated,
				BlockHeight:    blockHeight,
				Classification: UTXOClassPending,
				CreatedAt:      now,
			},
		); err != nil {
			return err
		}
	}

	for _, u := range diff.spent {
		if _, err := a.writeAuditRow(
			ctx, WalletUTXOLogEntry{
				Outpoint:       u.Outpoint,
				Amount:         u.Amount,
				Event:          UTXOAuditSpent,
				BlockHeight:    blockHeight,
				Classification: UTXOClassPending,
				CreatedAt:      now,
			},
		); err != nil {
			return err
		}
	}

	return nil
}

// reconcilePendingAuditRows promotes audit rows the previous
// block's diff left in the 'pending' limbo state. A row is
// eligible for promotion when its block_height is strictly
// below the current block_height, which gives the matching
// RoundConfirmedMsg / SweepCompletedMsg a full block window
// to land on the actor's mailbox and turn the pending row
// into an attributed row before the classifier concludes the
// movement is genuinely external.
//
// Each promoted row triggers a RecordExternalDeposit (for
// created) or RecordExternalWithdrawal (for spent) ledger
// leg so treasury_wallet balance stays in sync with on-chain
// reality without double-counting rounds or sweeps.
//
// Atomicity: PromotePendingWalletUTXOLog and every
// RecordExternalDeposit / RecordExternalWithdrawal below run
// through TransactionExecutor.ExecTx, which joins the outer
// actor-framework transaction stashed on ctx by
// DurableActor.processInTransaction (see
// db/interfaces.go:ExecTx and
// client/baselib/actor/durable_actor.go:processInTransaction).
// If any bookExternalLeg call fails, the handler returns the
// error up to the durable actor, which rolls back the joined
// transaction -- including the UPDATE that promoted the
// pending rows. On the next BlockEpochMsg the rows are still
// 'pending' and the classifier retries naturally, so no
// external_* leg is dropped on transient persistence
// failures.
func (a *LedgerActor) reconcilePendingAuditRows(ctx context.Context,
	blockHeight int64) error {

	if !a.cfg.UTXOAuditStore.IsSome() {
		return nil
	}
	store := a.cfg.UTXOAuditStore.UnsafeFromSome()

	promoted, err := store.PromotePendingWalletUTXOLog(
		ctx, blockHeight,
	)
	if err != nil {
		return fmt.Errorf(
			"promote pending audit rows: %w", err,
		)
	}
	if len(promoted) == 0 {
		return nil
	}

	a.log.InfoS(ctx, "Promoting pending UTXO audit rows",
		slog.Int64("block_height", blockHeight),
		slog.Int("count", len(promoted)),
	)

	for _, entry := range promoted {
		if err := a.bookExternalLeg(ctx, entry); err != nil {
			return err
		}
	}

	return nil
}

// bookExternalLeg books the appropriate external_* ledger leg
// for a promoted audit row. The outpoint-derived idempotency
// key matches the client's exitIdempotencyKey layout so the
// two sides use the same shape.
func (a *LedgerActor) bookExternalLeg(ctx context.Context,
	entry WalletUTXOLogEntry) error {

	key := outpointKey(entry.Outpoint)
	at := a.clk.Now()

	switch entry.Event {
	case UTXOAuditCreated:
		return fees.RecordExternalDeposit(
			ctx, a.cfg.LedgerStore, key,
			entry.Amount, at,
		)

	case UTXOAuditSpent:
		return fees.RecordExternalWithdrawal(
			ctx, a.cfg.LedgerStore, key,
			entry.Amount, at,
		)

	default:
		return fmt.Errorf(
			"%w: unexpected audit event %q",
			ErrInvalidMessage, entry.Event,
		)
	}
}

// writeAuditRow persists one wallet_utxo_log row if an audit
// store is configured. A nil store is not an error: the diff
// subsystem still runs its in-memory snapshot tracking and the
// audit trail is simply disabled. The rowcount return
// distinguishes a freshly inserted row (1) from a silent
// no-op against an already-attributed row (0); callers use
// this signal to decide whether to book external_* ledger
// legs.
func (a *LedgerActor) writeAuditRow(ctx context.Context,
	entry WalletUTXOLogEntry) (int64, error) {

	if !a.cfg.UTXOAuditStore.IsSome() {
		return 0, nil
	}
	store := a.cfg.UTXOAuditStore.UnsafeFromSome()

	rows, err := store.InsertWalletUTXOLog(ctx, entry)
	if err != nil {
		return 0, fmt.Errorf(
			"insert wallet_utxo_log: %w", err,
		)
	}

	return rows, nil
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
