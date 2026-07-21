package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
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
// in db/sqlc/migrations/000006_accounting.up.sql.
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
	AccountWalletClearing = "wallet_clearing"
)

// Client-side ledger event types matching the seeded event types
// in the migration.
const (
	EventBoardingFeePaid      = "boarding_fee_paid"
	EventRefreshFeePaid       = "refresh_fee_paid"
	EventOnchainFeePaid       = "onchain_fee_paid"
	EventBoardingSweepFeePaid = "boarding_sweep_fee_paid"
	EventVTXOReceived         = "vtxo_received"
	EventVTXOSent             = "vtxo_sent"
	EventWalletUTXOCreated    = "wallet_utxo_created"
	EventWalletUTXOSpent      = "wallet_utxo_spent"
	EventWalletSweepTransfer  = "wallet_sweep_transfer"
)

// Canonical VTXOReceivedMsg.Source values. Callers must use one
// of these strings; any other value causes handleVTXOReceived to
// return an error rather than silently misclassify the entry.
//
//   - SourceRoundBoarding: VTXO is the result of the client boarding
//     its own on-chain wallet funds into a round. The offsetting
//     leg moves value from wallet_balance into vtxo_balance.
//   - SourceRoundRefresh: VTXO materialized as the output side of
//     a round in which the client also forfeited VTXOs of roughly
//     equal value (refresh or directed-send self-change). The
//     offsetting leg credits transfers_out so it cancels with the
//     companion VTXOSentMsg's transfers_out debit; the net effect
//     on vtxo_balance is just the operator fee, booked separately
//     via FeePaidMsg. This avoids spuriously crediting
//     wallet_balance on a flow that never touches the on-chain
//     wallet.
//   - SourceRoundTransfer: VTXO was received from another round
//     participant (in-round transfer). The offsetting leg credits
//     transfers_in.
//   - SourceOOR: VTXO was received out-of-round (OOR transfer from
//     another participant). The offsetting leg credits transfers_in.
const (
	SourceRoundBoarding = "round_boarding"
	SourceRoundRefresh  = "round_refresh"
	SourceRoundTransfer = "round_transfer"
	SourceOOR           = "oor"
	SourceClaimReissue  = "claim_reissue"
)

// Canonical FeePaidMsg.FeeType values. A misspelled or missing
// fee type is rejected by handleFeePaid with an explicit error
// so durable-mailbox replays surface caller bugs loudly instead
// of silently misclassifying the entry.
//
// FeeTypeBoarding and FeeTypeRefresh book operator (Ark protocol)
// fees, debiting fees_paid. The credit side names the account the
// fee was paid from: wallet_balance for boarding (the fee comes out
// of the wallet funds entering the Ark layer, alongside a
// same-RoundID VTXOReceivedMsg carrying the SEALED post-fee VTXO
// value) and vtxo_balance for refresh (the fee is carved out of
// forfeited VTXO value).
//
// FeeTypeOnchainSweep books wallet-level sweep chain cost
// (currently emitted by the boarding-sweep flow): debit
// onchain_fees, credit wallet_clearing. RoundID may be zero —
// the fee is keyed by sweep txid via IdempotencyKey instead.
const (
	FeeTypeBoarding     = "boarding"
	FeeTypeRefresh      = "refresh"
	FeeTypeOnchainSweep = "onchain_sweep"
)

// Canonical Classification values seeded by migrations specific to
// boarding-sweep events. Callers must use these strings rather than
// literals so the seed table and producer agree.
const (
	ClassificationBoardingSweepInput  = "boarding_sweep_input"
	ClassificationBoardingSweepReturn = "boarding_sweep_return"
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

	// ChainTxid optionally links this ledger entry to an on-chain
	// transaction. It is a first-class history field rather than a
	// value callers must recover from Description or IdempotencyKey.
	ChainTxid []byte

	// ChainVout optionally records the output index within ChainTxid.
	// Nil means this ledger entry is not tied to a concrete output.
	ChainVout *int32

	// ConfirmationHeight optionally records the block height that
	// confirmed the on-chain transaction. Nil means the height is
	// unknown or the ledger entry has no chain transaction.
	ConfirmationHeight *int32
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
	// deterministic clock (shared with the rest of waved so
	// every persisted row pins to the same test frame).
	Clock fn.Option[clock.Clock]
}

// ledgerTx is the typed, transaction-scoped store handed to ledger handlers
// inside a single Commit. Every write a handler issues through it joins that
// one Commit transaction (the stores pick up the *sql.Tx from the handler's
// context), so multi-leg events such as ExitCost book all legs atomically with
// the mailbox ack. This is the store type the durable-actor Exec handle injects
// per message via NewLedgerActor's StoreFactory.
type ledgerTx struct {
	// ledger persists double-entry ledger rows.
	ledger LedgerStore

	// audit persists wallet UTXO audit-log rows. Nil when the actor runs in
	// log-only mode (no UTXOAuditStore configured).
	audit UTXOAuditStore
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

var (
	// Compile-time check that LedgerActor implements the durable
	// transaction-aware behavior interface over its typed ledgerTx store.
	_ actor.TxBehavior[LedgerMsg, LedgerResp, ledgerTx] = (*LedgerActor)(nil)

	// Compile-time check that LedgerActor implements actor-system
	// cleanup.
	_ actor.Stoppable = (*LedgerActor)(nil)
)

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

	// Run on the durable actor's Read/Commit execution path: each handler
	// books its ledger legs inside one short, lease-fenced Commit
	// transaction rather than the framework holding a writer tx across the
	// whole Receive. The StoreFactory injects the typed ledgerTx store
	// bound to that Commit transaction.
	//
	// The durable actor should not see LedgerActor's OnStop hook, because
	// that hook waits for the durable actor itself to exit. The
	// actor-system wrapper owns that cleanup instead.
	durableCfg := actor.DefaultDurableTxActorConfig[
		LedgerMsg, LedgerResp, ledgerTx,
	](
		a.actorID, a, a.bindStores, a.cfg.DeliveryStore, codec,
	)
	durable, err := actor.NewDurableActor(durableCfg).Unpack()
	if err != nil {
		return fmt.Errorf("build ledger durable actor: %w", err)
	}
	a.durable = durable
	a.ref = a.durable.Ref()

	checkpoint, err := a.cfg.DeliveryStore.LoadCheckpoint(
		ctx, a.actorID,
	)
	if err != nil {
		return fmt.Errorf("load checkpoint: %w", err)
	}

	err = actor.PrependRestartMessage(
		ctx, a.cfg.DeliveryStore, codec, a.actorID, checkpoint,
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

// OnStop implements actor.Stoppable by stopping the durable ledger runtime and
// waiting for it to exit before the actor system closes shared storage.
func (a *LedgerActor) OnStop(ctx context.Context) error {
	if a.durable == nil {
		return nil
	}

	return a.durable.StopAndWait(ctx)
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
func (a *LedgerActor) logHandlerErr(ctx context.Context, msg string,
	err error) {

	attrs := []any{slog.String("actor_id", a.actorID)}
	a.log.WarnS(ctx, msg, err, attrs...)

	// Cheaply keep the branch on whether this was a validation
	// reject so future monitoring that wants to split the two
	// cases can pick up the distinction without replaying the
	// whole log line.
	_ = errors.Is(err, ErrInvalidMessage)
}

// bindStores is the StoreFactory passed to the durable actor: it produces the
// typed ledgerTx store the Exec handle hands to each handler inside a Commit.
// The stores join the Commit transaction via the context passed to their
// methods, so the factory itself ignores its arguments and just wires the
// configured stores through.
func (a *LedgerActor) bindStores(_ context.Context,
	_ actor.DeliveryStore) ledgerTx {

	return ledgerTx{
		ledger: a.cfg.LedgerStore,
		audit:  a.cfg.UTXOAuditStore,
	}
}

// Receive processes one durable message. This is the TxBehavior implementation
// called by the durable runtime: each handler that persists state runs inside a
// single lease-fenced Commit transaction via the Exec handle, so the ledger
// writes and the mailbox ack land atomically.
func (a *LedgerActor) Receive(ctx context.Context, msg LedgerMsg,
	ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	switch m := msg.(type) {
	case *actor.RestartMessage:
		// Restart carries no state to persist, so it does not Commit;
		// the framework consumes it via the non-transactional ack path.
		a.log.InfoS(ctx, "Ledger actor restarted")

		return fn.Ok[LedgerResp](nil)

	case *FeePaidMsg:
		return a.handleFeePaid(ctx, m, ax)

	case *VTXOReceivedMsg:
		return a.handleVTXOReceived(ctx, m, ax)

	case *VTXOSentMsg:
		return a.handleVTXOSent(ctx, m, ax)

	case *VTXOClaimReissuedMsg:
		return a.handleVTXOClaimReissued(ctx, m, ax)

	case *ExitCostMsg:
		return a.handleExitCost(ctx, m, ax)

	case *UTXOCreatedMsg:
		return a.handleUTXOCreated(ctx, m, ax)

	case *UTXOSpentMsg:
		return a.handleUTXOSpent(ctx, m, ax)

	case *BoardingSweepConfirmedMsg:
		return a.handleBoardingSweepConfirmed(ctx, m, ax)

	default:
		return fn.Err[LedgerResp](
			fmt.Errorf("%w: unknown message type: %T",
				ErrInvalidMessage, msg),
		)
	}
}

// fail logs a pre-Commit handler failure (validation or message-building) and
// returns it as the result. These failures happen before any Commit, so they
// do not flow through commit's logging below; logging here keeps the operator
// view uniform. ErrLeaseLost never originates from this path.
func (a *LedgerActor) fail(ctx context.Context, errMsg string,
	err error) fn.Result[LedgerResp] {

	a.logHandlerErr(ctx, errMsg, err)

	return fn.Err[LedgerResp](err)
}

// commit runs the write closure inside one lease-fenced Commit transaction and
// maps the outcome to a LedgerResp result. Handlers do their validation and
// entry construction BEFORE calling commit, so the closure passed here contains
// only the persistence (InsertLedgerEntry / InsertUTXOAuditEntry) and the
// writer lock is held for nothing else. Commit failures are logged at WarnS via
// logHandlerErr. A lost lease (ErrLeaseLost) is not a handler fault -- the
// message was reclaimed and reprocessed elsewhere -- so it is returned without
// the handler-error log; the framework's retry path takes over.
func (a *LedgerActor) commit(ctx context.Context, ax actor.Exec[ledgerTx],
	errMsg string,
	write func(ctx context.Context, q ledgerTx) error,
) fn.Result[LedgerResp] {

	if err := ax.Commit(ctx, write); err != nil {
		if !errors.Is(err, actor.ErrLeaseLost) {
			a.logHandlerErr(ctx, errMsg, err)
		}

		return fn.Err[LedgerResp](err)
	}

	return fn.Ok[LedgerResp](nil)
}
