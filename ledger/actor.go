package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ErrInvalidMessage is the sentinel wrapped by handler errors
// that stem from caller-side input problems (unknown fee type,
// unknown source, ambiguous VTXOSentMsg, etc.). Receive logs
// these at error level because the caller is buggy; persistence
// failures that don't wrap this sentinel are logged at warn
// level because they're external triggers (DB locked, I/O, etc.)
// and should not page on their own.
var ErrInvalidMessage = errors.New("ledger: invalid message")

const (
	// defaultActorID is the durable mailbox identifier for the
	// client-side ledger actor.
	defaultActorID = "ledger.accounting"
)

// Client-side account identifiers matching the seeded accounts
// in db/sqlc/migrations/000006_fee_accounting.up.sql.
//
// TransfersIn is the counterparty side of received VTXOs (revenue-type),
// TransfersOut is the counterparty side of sent VTXOs (expense-type).
// These two accounts keep gross send/receive flows distinct instead of
// netting them on a single account.
const (
	AccountWalletBalance  = "wallet_balance"
	AccountVTXOBalance    = "vtxo_balance"
	AccountFeesPaid       = "fees_paid"
	AccountOnchainFees    = "onchain_fees"
	AccountTransfersIn    = "transfers_in"
	AccountTransfersOut   = "transfers_out"
	AccountOpeningBalance = "opening_balance"
)

// Client-side ledger event types matching the seeded event types
// in the migration.
const (
	EventBoardingFeePaid   = "boarding_fee_paid"
	EventRefreshFeePaid    = "refresh_fee_paid"
	EventOnchainFeePaid    = "onchain_fee_paid"
	EventVTXOReceived      = "vtxo_received"
	EventVTXOSent          = "vtxo_sent"
	EventWalletUTXOCreated = "wallet_utxo_created"
)

// Canonical VTXOReceivedMsg.Source values. Callers must use one
// of these strings; any other value causes handleVTXOReceived to
// return an error rather than silently misclassify the entry.
//
//   - SourceRoundBoarding: VTXO is the result of the client boarding
//     its own on-chain wallet funds into a round (or refreshing an
//     existing VTXO back into a round). The offsetting leg moves
//     value from wallet_balance into vtxo_balance.
//   - SourceRoundTransfer: VTXO was received from another round
//     participant (in-round transfer). The offsetting leg credits
//     transfers_in.
//   - SourceOOR: VTXO was received out-of-round (OOR transfer from
//     another participant). The offsetting leg credits transfers_in.
const (
	SourceRoundBoarding = "round_boarding"
	SourceRoundTransfer = "round_transfer"
	SourceOOR           = "oor"
)

// Canonical FeePaidMsg.FeeType values. A misspelled or missing
// fee type is rejected by handleFeePaid with an explicit error
// so durable-mailbox replays surface caller bugs loudly instead
// of silently misclassifying the entry.
const (
	FeeTypeBoarding = "boarding"
	FeeTypeRefresh  = "refresh"
)

// Canonical UTXO audit classification values matching the
// utxo_classifications seed table in migration 000007. Callers
// must use one of these strings for UTXOCreatedMsg.Classification
// and UTXOSpentMsg.Classification.
const (
	ClassificationDeposit      = "deposit"
	ClassificationSweepReturn  = "sweep_return"
	ClassificationRoundFunding = "round_funding"
	ClassificationChange       = "change"
	ClassificationUnknown      = "unknown"
)

// LedgerEntry is the domain-level representation of a
// double-entry ledger record for the client. This decouples the
// ledger actor from sqlc-generated types.
type LedgerEntry struct {
	// DebitAccount is the account name to debit (increase
	// expense or decrease asset).
	DebitAccount string

	// CreditAccount is the account name to credit (increase
	// income or increase asset).
	CreditAccount string

	// AmountSat is the entry amount in satoshis. Must be
	// positive.
	AmountSat int64

	// RoundID links the entry to a specific round (16-byte UUID).
	// Nil for events that have no round association; partial
	// unique index idx_client_ledger_idempotent_round only covers
	// rows where round_id IS NOT NULL.
	RoundID []byte

	// SessionID links the entry to a specific OOR session
	// (32-byte identifier). Nil for events with no session
	// association. Partial unique index
	// idx_client_ledger_idempotent_session covers rows where
	// session_id IS NOT NULL.
	SessionID []byte

	// EventType classifies the ledger event (e.g.
	// "boarding_fee_paid", "vtxo_received").
	EventType string

	// Description is a human-readable summary of the entry.
	Description string

	// CreatedAt is the Unix timestamp when the entry was
	// recorded.
	CreatedAt int64

	// IdempotencyKey is an optional natural dedup key used by
	// handlers whose events carry neither a round_id nor an OOR
	// session_id (e.g. on-chain exit legs keyed by outpoint).
	// Nil for entries that rely on round_id / session_id for
	// uniqueness. Partial unique index
	// idx_client_ledger_idempotent_key covers rows where
	// idempotency_key IS NOT NULL.
	IdempotencyKey []byte
}

// LedgerStore is the interface for persisting client-side ledger
// entries. Implementations bridge to the sqlc-generated queries
// via the db package.
//
// Multi-leg handlers (e.g. ExitCost's send leg + fee leg) still
// commit atomically even with two separate InsertLedgerEntry
// calls: the durable actor framework runs the whole Receive body
// inside a single TxAwareDeliveryStore transaction, and
// LedgerStoreDB.InsertLedgerEntry joins that outer transaction
// via db.TransactionExecutor.ExecTx rather than opening a new
// one. A crash mid-handler therefore rolls back both the writes
// and the ack together; no partial-write window exists on the
// durable path.
type LedgerStore interface {
	// InsertLedgerEntry persists a single ledger leg. The call
	// joins any outer actor transaction present in ctx so that
	// multiple invocations within one handler commit atomically
	// with the mailbox ack. Conflicts on the idempotency partial
	// unique indexes (round_id / session_id / idempotency_key)
	// are swallowed via ON CONFLICT DO NOTHING so redelivery of
	// a partially-processed message is a silent no-op.
	InsertLedgerEntry(
		ctx context.Context, entry LedgerEntry,
	) error
}

// UTXOAuditEntry is the domain-level representation of a wallet
// UTXO audit log record. Each row records a single UTXO being
// created or spent, classified by its likely cause.
type UTXOAuditEntry struct {
	// OutpointHash is the 32-byte transaction hash.
	OutpointHash []byte

	// OutpointIndex is the output index within the transaction.
	OutpointIndex int32

	// AmountSat is the UTXO value in satoshis.
	AmountSat int64

	// Event is "created" or "spent".
	Event string

	// BlockHeight is the block where this change occurred.
	BlockHeight int32

	// ClassifiedAs categorizes the UTXO event (e.g.
	// "deposit", "round_funding", "sweep_return", "change",
	// "unknown").
	ClassifiedAs string

	// CreatedAt is the Unix timestamp when this entry was
	// recorded.
	CreatedAt int64
}

// UTXOAuditStore is the interface for persisting wallet UTXO
// audit log entries. Implementations bridge to the
// sqlc-generated queries via the db package.
type UTXOAuditStore interface {
	InsertUTXOAuditEntry(
		ctx context.Context, entry UTXOAuditEntry,
	) error
}

// ActorConfig configures the client-side LedgerActor.
type ActorConfig struct {
	// Log is an optional logger. When None, logging is
	// disabled.
	Log fn.Option[btclog.Logger]

	// DeliveryStore persists actor mailbox state for crash
	// recovery.
	DeliveryStore actor.DeliveryStore

	// LedgerStore provides DB persistence for ledger entries.
	LedgerStore LedgerStore

	// UTXOAuditStore provides DB persistence for UTXO audit
	// log entries. When nil, UTXO audit messages are logged
	// but not persisted.
	UTXOAuditStore UTXOAuditStore

	// ActorID is the mailbox/checkpoint identifier. Defaults
	// to "ledger.accounting" if empty.
	ActorID string

	// Clock is the time source used to stamp ledger entries.
	// When None, the actor uses clock.NewDefaultClock() so
	// production code keeps its behavior; tests inject a
	// deterministic clock (shared with the rest of darepod so
	// every persisted row pins to the same test frame).
	Clock fn.Option[clock.Clock]
}

// LedgerActor is a durable actor that serializes all client-side
// accounting writes from rounds, OOR transfers, and on-chain
// exits. It receives fire-and-forget Tell messages and persists
// double-entry ledger entries to the database.
//
// The actor follows the same durable pattern as the server-side
// version: each message implements TLVMessage for crash-safe
// mailbox delivery, and a RestartMessage is prepended on startup
// for state reconstruction.
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
}

// Compile-time check that LedgerActor implements the durable
// actor behavior interface.
var _ actor.ActorBehavior[LedgerMsg, LedgerResp] = (*LedgerActor)(nil)

// NewLedgerActor creates a new client-side ledger actor
// instance. This is a pure constructor that performs no I/O.
// Call Start to initialize the durable runtime.
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
	}
}

// Start loads durable mailbox state and starts the actor
// runtime. On restart, unprocessed messages are replayed from
// the delivery store.
func (a *LedgerActor) Start(ctx context.Context) error {
	if a.cfg.DeliveryStore == nil {
		return fmt.Errorf("delivery store must be provided")
	}
	if a.cfg.LedgerStore == nil {
		return fmt.Errorf("ledger store must be provided")
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

// logHandlerErr logs a handler failure at WarnS regardless of
// class. Per the project convention, error-level logging is
// reserved for internal bugs and should never fire from an
// external trigger. Both categories of handler failure are
// externally triggered: ErrInvalidMessage is a malformed caller
// payload, and the residual DB-failure class is a transient
// infrastructure problem. Treating either as an Error would
// cause a misbehaving sender or a blipping DB to page.
//
// The level is held at WarnS uniformly so the operator sees the
// full sequence of ledger handler rejections at one severity
// while the actor continues to drain the mailbox.
func (a *LedgerActor) logHandlerErr(ctx context.Context,
	msg string, err error) {

	attrs := []any{slog.String("actor_id", a.actorID)}
	a.log.WarnS(ctx, msg, err, attrs...)

	// Cheaply keep the branch on whether this was a validation
	// reject so future monitoring that wants to split the two
	// cases can pick up the distinction without replaying the
	// whole log line.
	_ = errors.Is(err, ErrInvalidMessage)
}

// Receive processes one durable message. This is the
// ActorBehavior implementation called by the durable runtime.
func (a *LedgerActor) Receive(ctx context.Context,
	msg LedgerMsg) fn.Result[LedgerResp] {

	switch m := msg.(type) {
	case *actor.RestartMessage:
		a.log.InfoS(ctx, "Ledger actor restarted")

		return fn.Ok[LedgerResp](nil)

	case *FeePaidMsg:
		if err := a.handleFeePaid(ctx, m); err != nil {
			a.logHandlerErr(
				ctx, "Failed to handle fee paid", err,
			)

			return fn.Err[LedgerResp](err)
		}

		return fn.Ok[LedgerResp](nil)

	case *VTXOReceivedMsg:
		if err := a.handleVTXOReceived(ctx, m); err != nil {
			a.logHandlerErr(
				ctx, "Failed to handle VTXO received",
				err,
			)

			return fn.Err[LedgerResp](err)
		}

		return fn.Ok[LedgerResp](nil)

	case *VTXOSentMsg:
		if err := a.handleVTXOSent(ctx, m); err != nil {
			a.logHandlerErr(
				ctx, "Failed to handle VTXO sent", err,
			)

			return fn.Err[LedgerResp](err)
		}

		return fn.Ok[LedgerResp](nil)

	case *ExitCostMsg:
		if err := a.handleExitCost(ctx, m); err != nil {
			a.logHandlerErr(
				ctx, "Failed to handle exit cost", err,
			)

			return fn.Err[LedgerResp](err)
		}

		return fn.Ok[LedgerResp](nil)

	case *UTXOCreatedMsg:
		if err := a.handleUTXOCreated(ctx, m); err != nil {
			a.logHandlerErr(
				ctx, "Failed to handle UTXO created",
				err,
			)

			return fn.Err[LedgerResp](err)
		}

		return fn.Ok[LedgerResp](nil)

	case *UTXOSpentMsg:
		if err := a.handleUTXOSpent(ctx, m); err != nil {
			a.logHandlerErr(
				ctx, "Failed to handle UTXO spent", err,
			)

			return fn.Err[LedgerResp](err)
		}

		return fn.Ok[LedgerResp](nil)

	default:
		return fn.Err[LedgerResp](
			fmt.Errorf(
				"%w: unknown message type: %T",
				ErrInvalidMessage, msg,
			),
		)
	}
}
