package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
var _ actor.ActorBehavior[LedgerMsg, LedgerResp] = (
	*LedgerActor)(nil)

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
func (a *LedgerActor) Start(ctx context.Context) error {
	if a.cfg.DeliveryStore == nil {
		return fmt.Errorf("delivery store must be provided")
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
