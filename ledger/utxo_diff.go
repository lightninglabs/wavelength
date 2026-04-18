package ledger

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"
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

// utxoSnapshot is the in-memory mirror of the previous block's
// wallet UTXO set, keyed by outpoint for O(1) diff lookups.
type utxoSnapshot map[wire.OutPoint]btcutil.Amount

// utxoTracker holds the actor's running snapshot of the
// treasury wallet UTXO set. The first handleBlockEpoch after
// startup seeds `prev` from the wallet; subsequent calls diff
// against `prev` and then overwrite it. Protected by `mu` so
// future callers (tests, diagnostics) can read the snapshot
// concurrently with a block-epoch processor.
type utxoTracker struct {
	mu   sync.Mutex
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

// applyUTXODiff writes audit rows and ledger legs for each
// created/spent entry. Classification is currently the naive
// "everything unknown is external" policy: this is intentional
// for the initial implementation since round/sweep tracking
// state is not yet available. When a later PR wires that
// tracking, the classifier here is the single point to update.
func (a *LedgerActor) applyUTXODiff(ctx context.Context,
	diff diffResult, blockHeight int64,
	stampDeposits bool) error {

	now := a.clk.Now()

	// Created UTXOs.
	for _, u := range diff.created {
		cls := UTXOClassDeposit
		if err := a.writeAuditRow(ctx, WalletUTXOLogEntry{
			Outpoint:       u.Outpoint,
			Amount:         u.Amount,
			Event:          UTXOAuditCreated,
			BlockHeight:    blockHeight,
			Classification: cls,
			CreatedAt:      now,
		}); err != nil {
			return err
		}

		// Skip ledger-side booking on the initial seeding
		// pass: those UTXOs existed before the actor
		// started tracking and already have a prior origin
		// story somewhere else.
		if !stampDeposits {
			continue
		}

		err := fees.RecordExternalDeposit(
			ctx, a.cfg.LedgerStore, outpointKey(u.Outpoint),
			u.Amount, now,
		)
		if err != nil {
			return fmt.Errorf(
				"record external_deposit: %w", err,
			)
		}
	}

	// Spent UTXOs.
	for _, u := range diff.spent {
		cls := UTXOClassUnknown
		if err := a.writeAuditRow(ctx, WalletUTXOLogEntry{
			Outpoint:       u.Outpoint,
			Amount:         u.Amount,
			Event:          UTXOAuditSpent,
			BlockHeight:    blockHeight,
			Classification: cls,
			CreatedAt:      now,
		}); err != nil {
			return err
		}

		// Same skip rule as for created: the first pass is
		// baseline reconstruction, not attributable
		// movement.
		if !stampDeposits {
			continue
		}

		err := fees.RecordExternalWithdrawal(
			ctx, a.cfg.LedgerStore, outpointKey(u.Outpoint),
			u.Amount, now,
		)
		if err != nil {
			return fmt.Errorf(
				"record external_withdrawal: %w", err,
			)
		}
	}

	return nil
}

// writeAuditRow persists one wallet_utxo_log row if an audit
// store is configured. A nil store is not an error: the diff
// subsystem still runs and books ledger entries; the audit
// trail is a supplementary observability layer.
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

// processBlockUTXODiff is the entry point invoked from
// handleBlockEpoch when the WalletUTXOLister is configured. It
// acquires the wallet's current UTXO set, computes the diff
// against the actor's previous snapshot, writes audit rows and
// external_* ledger legs, then replaces the snapshot.
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

	a.utxo.mu.Lock()
	defer a.utxo.mu.Unlock()

	stampDeposits := a.utxo.seeded
	diff := diffSnapshots(a.utxo.prev, current)

	a.log.InfoS(ctx, "Wallet UTXO diff",
		slog.Int64("block_height", blockHeight),
		slog.Int("created", len(diff.created)),
		slog.Int("spent", len(diff.spent)),
		slog.Bool("seeded", a.utxo.seeded),
	)

	if err := a.applyUTXODiff(
		ctx, diff, blockHeight, stampDeposits,
	); err != nil {
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
