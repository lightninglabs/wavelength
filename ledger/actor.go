package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// defaultActorID is the durable mailbox identifier for the
	// ledger actor.
	defaultActorID = "ledger.accounting"
)

// ErrInvalidMessage is returned by handlers when a received
// ledger message fails caller-facing validation (non-positive
// amount, malformed identifier, unknown enum value). Handlers
// wrap this sentinel so the Receive loop can distinguish
// externally-triggered bugs (log at WarnS, dead-letter the
// message) from internal failures (log at ErrorS).
var ErrInvalidMessage = errors.New("invalid ledger message")

// LedgerStore is the interface for persisting ledger entries.
// The db.Store type satisfies this via its embedded
// *sqlc.Queries.
type LedgerStore = fees.LedgerStore

// LedgerBalanceReader reports the current signed balance of a
// chart-of-accounts entry, computed from the persisted ledger
// (debits add, credits subtract). The ledger actor calls
// GetAccountBalance(AccountDeployedCapital) on Start to rehydrate
// the TreasuryTracker from durable state, so a process restart
// does not silently reset in-memory capital totals to zero and
// let congestion pricing fire on stale utilization numbers.
//
// The interface is decoupled from fees.LedgerStore (which is
// write-only by design, consumed by every Record* helper) so
// adding balance reads here does not widen the call surface of
// the recording path. db.LedgerStoreDB satisfies it via the
// sqlc-generated GetAccountBalance query, which runs under a
// read-only transaction.
type LedgerBalanceReader interface {
	GetAccountBalance(
		ctx context.Context, account fees.AccountID,
	) (btcutil.Amount, error)
}

// ActorConfig configures the LedgerActor.
type ActorConfig struct {
	// Log is an optional logger. When None, logging is disabled.
	Log fn.Option[btclog.Logger]

	// DeliveryStore persists actor mailbox state for crash
	// recovery.
	DeliveryStore actor.DeliveryStore

	// LedgerStore provides DB persistence for ledger entries.
	LedgerStore LedgerStore

	// TreasuryTracker is updated with capital deployment and
	// wallet balance changes.
	TreasuryTracker *fees.TreasuryTracker

	// BalanceReader lets Start rehydrate the TreasuryTracker
	// from persisted ledger totals before the mailbox accepts
	// messages. When None, the tracker stays at its
	// caller-initialized value (zeros if NewTreasuryTracker was
	// used without a subsequent Initialize) and congestion
	// pricing degrades to the pre-restart behavior: utilization
	// reads zero until new events re-populate the buckets. A
	// production deployment should always wire a reader so
	// congestion pricing converges to DB truth on startup.
	BalanceReader fn.Option[LedgerBalanceReader]

	// ActorID is the mailbox/checkpoint identifier. Defaults
	// to "ledger.accounting" if empty.
	ActorID string

	// Clock is the time source used to stamp ledger entries'
	// CreatedAt timestamps. When None, the actor falls back to
	// clock.NewDefaultClock() so production code keeps its
	// behavior; tests inject a deterministic clock so every
	// persisted row pins to the same test frame.
	Clock fn.Option[clock.Clock]

	// WalletUTXOLister produces the current treasury wallet
	// UTXO set on demand. Driven by handleBlockEpoch each
	// block to compute the created/spent diff. When None,
	// the diff subsystem is inert — handleBlockEpoch logs
	// the epoch and returns without touching the audit log
	// or the ledger. Concrete wiring to lndbackend lands in
	// a follow-up PR.
	WalletUTXOLister fn.Option[WalletUTXOLister]

	// UTXOAuditStore persists wallet_utxo_log rows written
	// by the UTXO diff subsystem. When None, audit rows are
	// skipped; the diff still runs and still books ledger
	// entries for unclassified movements.
	UTXOAuditStore fn.Option[UTXOAuditStore]

	// UTXOSnapshotReader rehydrates the actor's in-memory
	// UTXO diff snapshot from the persisted audit log on
	// Start. When None, the actor re-enters seeding mode on
	// every restart -- any external deposit that arrived
	// during downtime is silently swallowed by the seeding
	// skip rather than booked as external_deposit. Production
	// deployments with a WalletUTXOLister MUST also wire a
	// reader; tests that exercise the diff path in isolation
	// can leave it None to accept the zero-snapshot seeding
	// behavior.
	UTXOSnapshotReader fn.Option[UTXOSnapshotReader]
}

// LedgerActor is a durable actor that serializes all accounting
// writes from rounds, OOR, sweeper, and block epoch subsystems.
// It receives fire-and-forget Tell messages and persists
// double-entry ledger entries to the database.
//
// The actor follows the same durable pattern as the OOR
// TransferCoordinatorActor: each message implements TLVMessage
// for crash-safe mailbox delivery, and a RestartMessage is
// prepended on startup for state reconstruction.
type LedgerActor struct {
	cfg ActorConfig

	actorID string

	durable *actor.DurableActor[LedgerMsg, LedgerResp]
	ref     actor.ActorRef[LedgerMsg, LedgerResp]

	log btclog.Logger

	// clk is the resolved clock.Clock used to stamp every
	// CreatedAt on persisted ledger entries. Pulled out of the
	// config once at construction so handlers can call
	// a.clk.Now() without re-optioning the field on each
	// message.
	clk clock.Clock

	// utxo is the running snapshot of the treasury wallet
	// UTXO set used by the per-block diff subsystem.
	// Initialized empty; seeded by the first BlockEpochMsg
	// that arrives after a WalletUTXOLister is configured.
	utxo *utxoTracker
}

// Compile-time check that LedgerActor implements the durable
// actor behavior interface.
var _ actor.ActorBehavior[LedgerMsg, LedgerResp] = (*LedgerActor)(nil)

// NewLedgerActor creates a new ledger actor instance. This is a
// pure constructor that performs no I/O. Call Start to initialize
// the durable runtime.
func NewLedgerActor(cfg ActorConfig) *LedgerActor {
	actorID := cfg.ActorID
	if actorID == "" {
		actorID = defaultActorID
	}

	return &LedgerActor{
		cfg:     cfg,
		actorID: actorID,
		log:     cfg.Log.UnwrapOr(btclog.Disabled),
		clk:     cfg.Clock.UnwrapOr(clock.NewDefaultClock()),
		utxo:    newUTXOTracker(),
	}
}

// Start loads durable mailbox state and starts the actor
// runtime. On restart, unprocessed messages are replayed from
// the delivery store.
//
// Before the mailbox begins accepting messages, Start rehydrates
// the TreasuryTracker from the persisted ledger (when both a
// tracker and a BalanceReader are configured). Without this
// step, a process restart would leave the tracker at its zero
// initial state while the DB still holds the true deployed
// capital -- utilization would read zero, suppressing congestion
// pricing, until new round events slowly rebuild the in-memory
// counter. The reseed runs first so the first post-restart
// handler invocation already sees correct totals.
func (a *LedgerActor) Start(ctx context.Context) error {
	if a.cfg.DeliveryStore == nil {
		return fmt.Errorf("delivery store must be provided")
	}

	if err := a.reseedTreasuryTracker(ctx); err != nil {
		return fmt.Errorf("reseed treasury tracker: %w", err)
	}

	if err := a.reseedUTXOSnapshot(ctx); err != nil {
		return fmt.Errorf("reseed utxo snapshot: %w", err)
	}

	codec := newLedgerCodec()

	durableCfg := actor.DefaultDurableActorConfig[
		LedgerMsg, LedgerResp,
	](
		a.actorID, a, a.cfg.DeliveryStore, codec,
	)
	a.durable = actor.NewDurableActor(durableCfg)
	a.ref = a.durable.Ref()

	checkpoint, err := a.cfg.DeliveryStore.LoadCheckpoint(
		ctx, a.actorID,
	)
	if err != nil {
		return fmt.Errorf("load checkpoint: %w", err)
	}

	err = actor.PrependRestartMessage(
		ctx, a.cfg.DeliveryStore, codec,
		a.actorID, checkpoint,
	)
	if err != nil {
		return fmt.Errorf("prepend restart: %w", err)
	}

	a.durable.Start()

	a.log.InfoS(ctx, "Ledger actor started",
		slog.String("actor_id", a.actorID),
	)

	return nil
}

// reseedTreasuryTracker rebuilds the in-memory capital buckets
// from the persisted ledger before the actor begins accepting
// messages. No-op when either the tracker or the balance reader
// is absent -- callers without a production wiring (unit tests,
// isolated actor harnesses) are expected to pre-populate the
// tracker themselves or accept the zero-initialized default.
//
// Because the ledger does not distinguish the "pending-sweep"
// slice of deployed_capital from the live-VTXO slice, the full
// account balance is folded into deployedCapital on reseed with
// pendingSweepSat cleared to zero. Subsequent forfeit and sweep
// events re-establish the split. The live VTXO count is not
// derivable from the ledger alone (the schema tracks amounts,
// not counts) and is left at zero; the count catches up as new
// RoundConfirmedMsg events arrive.
func (a *LedgerActor) reseedTreasuryTracker(ctx context.Context) error {
	tracker := a.cfg.TreasuryTracker
	if tracker == nil {
		return nil
	}

	if a.cfg.BalanceReader.IsNone() {
		return nil
	}
	reader := a.cfg.BalanceReader.UnsafeFromSome()

	deployed, err := reader.GetAccountBalance(
		ctx, fees.AccountDeployedCapital,
	)
	if err != nil {
		return fmt.Errorf("read deployed_capital balance: %w", err)
	}

	wallet, err := reader.GetAccountBalance(
		ctx, fees.AccountTreasuryWallet,
	)
	if err != nil {
		return fmt.Errorf("read treasury_wallet balance: %w", err)
	}

	tracker.Reseed(int64(deployed), 0, 0, wallet)

	a.log.InfoS(ctx, "Treasury tracker reseeded from ledger",
		slog.Int64("deployed_capital_sat", int64(deployed)),
		slog.Int64("wallet_balance_sat", int64(wallet)),
	)

	return nil
}

// Stop stops the durable ledger actor.
func (a *LedgerActor) Stop() {
	if a.durable != nil {
		a.durable.Stop()
	}
}

// Ref returns the actor reference for sending messages.
func (a *LedgerActor) Ref() actor.ActorRef[LedgerMsg, LedgerResp] {
	return a.ref
}

// Receive processes one durable message. This is the
// ActorBehavior implementation called by the durable runtime.
//
// All handler failures are externally triggered (bad payload,
// DB constraint, transient persistence error) rather than
// internal bugs, so they log at WarnS. The error-log level is
// reserved for operator-facing bugs inside this package that
// would break the ledger — none exist on the normal code path.
func (a *LedgerActor) Receive(ctx context.Context,
	msg LedgerMsg) fn.Result[LedgerResp] {

	var (
		err     error
		failMsg string
	)

	switch m := msg.(type) {
	case *actor.RestartMessage:
		a.log.InfoS(ctx, "Ledger actor restarted")
		return fn.Ok[LedgerResp](nil)

	case *RoundConfirmedMsg:
		err = a.handleRoundConfirmed(ctx, m)
		failMsg = "Failed to handle round confirmed"

	case *VTXOsForfeitedMsg:
		err = a.handleVTXOsForfeited(ctx, m)
		failMsg = "Failed to handle VTXOs forfeited"

	case *SweepCompletedMsg:
		err = a.handleSweepCompleted(ctx, m)
		failMsg = "Failed to handle sweep completed"

	case *OORFinalizedMsg:
		err = a.handleOORFinalized(ctx, m)
		failMsg = "Failed to handle OOR finalized"

	case *BlockEpochMsg:
		err = a.handleBlockEpoch(ctx, m)
		failMsg = "Failed to handle block epoch"

	default:
		return fn.Err[LedgerResp](
			fmt.Errorf("%w: unknown message type: %T",
				ErrInvalidMessage, msg),
		)
	}

	if err != nil {
		a.log.WarnS(ctx, failMsg, err)
		return fn.Err[LedgerResp](err)
	}

	return fn.Ok[LedgerResp](nil)
}
