package ledgeractor

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo/fees"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// defaultActorID is the durable mailbox identifier for the
	// ledger actor.
	defaultActorID = "ledger.accounting"
)

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
func (a *LedgerActor) Receive(ctx context.Context,
	msg LedgerMsg) fn.Result[LedgerResp] {

	switch m := msg.(type) {
	case *actor.RestartMessage:
		a.log.InfoS(ctx, "Ledger actor restarted")

		return fn.Ok[LedgerResp](nil)

	case *RoundConfirmedMsg:
		if err := a.handleRoundConfirmed(ctx, m); err != nil {
			a.log.ErrorS(ctx,
				"Failed to handle round confirmed",
				err,
			)

			return fn.Err[LedgerResp](err)
		}

		return fn.Ok[LedgerResp](nil)

	case *VTXOsForfeitedMsg:
		if err := a.handleVTXOsForfeited(ctx, m); err != nil {
			a.log.ErrorS(ctx,
				"Failed to handle VTXOs forfeited",
				err,
			)

			return fn.Err[LedgerResp](err)
		}

		return fn.Ok[LedgerResp](nil)

	case *SweepCompletedMsg:
		if err := a.handleSweepCompleted(ctx, m); err != nil {
			a.log.ErrorS(ctx,
				"Failed to handle sweep completed",
				err,
			)

			return fn.Err[LedgerResp](err)
		}

		return fn.Ok[LedgerResp](nil)

	case *OORFinalizedMsg:
		if err := a.handleOORFinalized(ctx, m); err != nil {
			a.log.ErrorS(ctx,
				"Failed to handle OOR finalized",
				err,
			)

			return fn.Err[LedgerResp](err)
		}

		return fn.Ok[LedgerResp](nil)

	case *BlockEpochMsg:
		if err := a.handleBlockEpoch(ctx, m); err != nil {
			a.log.ErrorS(ctx,
				"Failed to handle block epoch",
				err,
			)

			return fn.Err[LedgerResp](err)
		}

		return fn.Ok[LedgerResp](nil)

	default:
		return fn.Err[LedgerResp](
			fmt.Errorf("unknown message type: %T", msg),
		)
	}
}
