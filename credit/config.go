package credit

import (
	"context"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/timeout"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ServerCreditState is the externally visible server credit operation state.
// The values mirror the swap server's CreditOperationState enum so the credit
// actor can reconcile against ListCredits without importing the swap RPC stubs.
type ServerCreditState string

const (
	// ServerStateCreated means the operation exists but is not funded.
	ServerStateCreated ServerCreditState = "created"

	// ServerStateAwaitingPayment means the operation is awaiting its
	// inbound payment (Lightning settlement or OOR top-up).
	ServerStateAwaitingPayment ServerCreditState = "awaiting_payment"

	// ServerStateCredited means value has been finalized into the account.
	ServerStateCredited ServerCreditState = "credited"

	// ServerStateReserved means credits are reserved for a pay/redeem op.
	ServerStateReserved ServerCreditState = "reserved"

	// ServerStatePayingLightning means the server is paying the invoice.
	ServerStatePayingLightning ServerCreditState = "paying_lightning"

	// ServerStateDebited means the reservation was finalized as a debit.
	ServerStateDebited ServerCreditState = "debited"

	// ServerStateSendingOOR means a redemption OOR is being sent.
	ServerStateSendingOOR ServerCreditState = "sending_oor"

	// ServerStateRedeemed means a redemption completed.
	ServerStateRedeemed ServerCreditState = "redeemed"

	// ServerStateReleased means a reservation was released without a debit.
	ServerStateReleased ServerCreditState = "released"

	// ServerStateExpired means the operation expired before completion.
	ServerStateExpired ServerCreditState = "expired"

	// ServerStateFailed means the operation failed terminally.
	ServerStateFailed ServerCreditState = "failed"
)

// IsTerminalFailure reports whether a server credit state is a terminal failure
// that the client must classify as a deterministic op failure rather than
// retry.
func (s ServerCreditState) IsTerminalFailure() bool {
	return s == ServerStateFailed || s == ServerStateExpired ||
		s == ServerStateReleased
}

// CreditSource identifies how value enters a credit account.
type CreditSource uint8

const (
	// SourceLightningReceive funds a credit via a server-owned Lightning
	// invoice.
	SourceLightningReceive CreditSource = iota + 1

	// SourceArkTopUp funds a credit via an OOR transfer to a server
	// destination.
	SourceArkTopUp
)

// CreateCreditResult is the server response to a CreateCredit call.
type CreateCreditResult struct {
	// OperationID is the server credit operation id.
	OperationID string

	// State is the current server operation state.
	State ServerCreditState

	// Invoice is the server-owned Lightning invoice (LIGHTNING_RECEIVE).
	Invoice string

	// PaymentHash is the invoice payment hash, when present.
	PaymentHash []byte

	// AmountSat is the amount that will be credited when complete.
	AmountSat uint64

	// DestinationPubkey is the server-owned Ark destination (ARK_TOPUP).
	DestinationPubkey []byte
}

// ServerCreditOp is one server credit operation from a ListCredits snapshot.
type ServerCreditOp struct {
	// OperationID is the server credit operation id.
	OperationID string

	// State is the current server operation state.
	State ServerCreditState

	// PaymentHash is the Lightning payment hash the operation is bound to,
	// when present (a pay or receive operation). It lets the credit actor
	// correlate a credit-only pay to its server pay operation without a
	// server-returned operation id.
	PaymentHash []byte
}

// CreditSnapshot is the server-authoritative account view.
type CreditSnapshot struct {
	// FinalizedSat is credits minus finalized debits.
	FinalizedSat uint64

	// ReservedSat is the sum of active holds.
	ReservedSat uint64

	// AvailableSat is the balance the server can reserve now.
	AvailableSat uint64

	// Operations are the recent server credit operations.
	Operations []ServerCreditOp
}

// RedeemResult is the server response to a RedeemCredit call.
type RedeemResult struct {
	// OperationID is the server redemption operation id.
	OperationID string

	// State is the current server operation state.
	State ServerCreditState

	// RedeemedSat is the amount materialized back into an Ark output.
	RedeemedSat uint64
}

// CreditServer is the swap-server credit and pay surface the credit actor uses.
// Every method is idempotent by the supplied idempotency key (CreateCredit /
// RedeemCredit) or by the invoice payment hash (StartPay), so a crashed turn
// re-issuing the call reconciles against the same server operation.
type CreditServer interface {
	// CreateCredit starts (or returns the existing) server credit funding
	// operation for this idempotency key.
	CreateCredit(ctx context.Context, accountPubKey []byte,
		idempotencyKey string, source CreditSource, amountSat uint64,
		memo string) (*CreateCreditResult, error)

	// ListCredits returns the server-authoritative account snapshot.
	ListCredits(ctx context.Context,
		accountPubKey []byte) (*CreditSnapshot, error)

	// RedeemCredit reserves available credits and sends them to the
	// supplied Ark destination.
	RedeemCredit(ctx context.Context, accountPubKey []byte,
		idempotencyKey string, amountSat uint64,
		destinationPubKey []byte) (*RedeemResult, error)

	// StartPay starts (or reuses, by payment hash) a credit/mixed pay for
	// the invoice with the given credit cap. A credit-only pay is then
	// reconciled to settlement by matching the invoice payment hash against
	// the pay operation surfaced in ListCredits.
	StartPay(ctx context.Context, invoice string, maxFeeSat,
		maxCreditSat uint64) error
}

// Store is the durable control-plane store the credit actors read and write.
// *db.CreditOperationStoreDB satisfies it in production; tests supply an
// in-memory fake.
type Store interface {
	// GetOperation loads one operation by op id, returning
	// db.ErrCreditOperationNotFound when absent.
	GetOperation(ctx context.Context,
		opID string) (*db.CreditOperationRecord, error)

	// UpsertOperation persists or updates one operation row.
	UpsertOperation(ctx context.Context, rec db.CreditOperationRecord) error

	// LookupActiveOperationByKey loads the non-failed operation row
	// carrying the given op key, returning db.ErrCreditOperationNotFound
	// when absent.
	LookupActiveOperationByKey(ctx context.Context,
		key string) (*db.CreditOperationRecord, error)

	// ListNonTerminal loads every non-terminal operation row for the
	// boot-time restore.
	ListNonTerminal(ctx context.Context) ([]db.CreditOperationRecord, error)

	// ListOperations loads every operation row, terminal and non-terminal
	// alike, so the wallet projector can observe terminal transitions.
	ListOperations(ctx context.Context) ([]db.CreditOperationRecord, error)
}

// CreditDaemon is the wallet/daemon surface the credit actor uses to fund
// top-ups, allocate redemption destinations, observe redeemed outputs, and read
// the operator dust limit.
type CreditDaemon interface {
	// IdentityPubKey returns the compressed wallet identity pubkey that
	// keys the server credit account.
	IdentityPubKey(ctx context.Context) ([]byte, error)

	// DustLimit returns the operator dust limit in satoshis.
	DustLimit(ctx context.Context) (uint64, error)

	// SendOOR submits an idempotency-keyed OOR transfer of amountSat to the
	// pubkey-backed destination and returns the OOR session id. The
	// supplied key dedups the transfer against the OOR registry, so a
	// re-issued send with the same key never produces a second transfer.
	SendOOR(ctx context.Context, destinationPubKey []byte, amountSat uint64,
		idempotencyKey string) (string, error)

	// AllocateReceiveScript allocates a fresh wallet-owned Ark receive
	// destination for a redemption payout, returning the x-only pubkey and
	// the pkScript.
	AllocateReceiveScript(ctx context.Context, label string) (pubKeyXOnly,
		pkScript []byte, err error)

	// FindLiveVTXOByPkScript reports whether a live VTXO matching pkScript
	// is indexed, and its amount.
	FindLiveVTXOByPkScript(ctx context.Context, pkScript []byte) (
		found bool, amountSat int64, err error)
}

// OpActorConfig configures one durable per-operation credit actor.
type OpActorConfig struct {
	// OpID is the stable operation id this actor owns. The durable mailbox
	// id is derived from it.
	OpID string

	// Log is an optional logger.
	Log fn.Option[btclog.Logger]

	// Server is the swap-server credit and pay surface.
	Server CreditServer

	// Daemon is the wallet/daemon surface.
	Daemon CreditDaemon

	// Store is the durable control-plane store for credit operations.
	Store Store

	// DeliveryStore backs the durable mailbox.
	DeliveryStore actor.DeliveryStore

	// TimeoutActor schedules retry/poll timers. When nil, awaiting states
	// do not self-poll and must be resumed explicitly.
	TimeoutActor actor.TellOnlyRef[timeout.Msg]

	// CallbackRef receives timeout expiries mapped into
	// ResumeCreditOpRequest.
	CallbackRef actor.TellOnlyRef[*timeout.ExpiredMsg]

	// Registry receives a CreditTerminalNotification after this op commits
	// a terminal snapshot, so the coordinator can reap the child.
	Registry actor.TellOnlyRef[CreditMsg]

	// PollInterval is the backoff between ListCredits / VTXO reconciliation
	// polls while awaiting a server or chain signal.
	PollInterval time.Duration

	// MaxAwaitingPolls caps how many reconciliation polls an awaiting state
	// may take before the operation terminal-fails, a backstop against an
	// operation parking forever when the server never reports a terminal
	// state. Zero means unlimited: rely on the server-reported terminal
	// states (expired/failed/released) to bound the wait.
	MaxAwaitingPolls uint32
}

// RegistryConfig configures the credit registry coordinator actor.
type RegistryConfig struct {
	// Log is an optional logger.
	Log fn.Option[btclog.Logger]

	// Server is the swap-server credit and pay surface shared by every
	// child.
	Server CreditServer

	// Daemon is the wallet/daemon surface shared by every child.
	Daemon CreditDaemon

	// Store is the durable control-plane store for credit operations.
	Store Store

	// DeliveryStore backs every per-operation child's durable mailbox. The
	// supervisor itself runs on a plain in-memory mailbox and does not use
	// it.
	DeliveryStore actor.DeliveryStore

	// TimeoutActor schedules retry/poll timers for children.
	TimeoutActor actor.TellOnlyRef[timeout.Msg]

	// CallbackRef receives timeout expiries mapped into
	// ResumeCreditOpRequest.
	CallbackRef actor.TellOnlyRef[*timeout.ExpiredMsg]

	// PollInterval is the backoff between reconciliation polls in children.
	PollInterval time.Duration

	// MaxAwaitingPolls caps reconciliation polls in an awaiting child state
	// before it terminal-fails. Zero relies on server-reported terminal
	// states to bound the wait.
	MaxAwaitingPolls uint32

	// AdmitTimeout bounds the synchronous server work an admission performs
	// on the supervisor goroutine (the receive CreateCredit call), so one
	// slow receive cannot head-of-line-block every other admission, resume,
	// and reap. Zero falls back to DefaultAdmitTimeout.
	AdmitTimeout time.Duration

	// AutoRedeem configures the wallet-owned auto-redeem policy.
	AutoRedeem AutoRedeemConfig

	// ActorSystem, when set, registers the registry under the credit
	// service key so a timeout retry-callback ref resolves to it. Nil in
	// unit tests that drive the registry ref directly.
	ActorSystem actor.SystemContext
}

// EarmarkFunc reports the credit balance reserved by in-flight wallet
// operations that have not yet written a durable credit_operations row. The
// auto-redeem sweep subtracts it from available credits so it never redeems
// credits a pending operation is about to spend.
type EarmarkFunc = func(context.Context) (uint64, error)

// AutoRedeemConfig configures the wallet-owned auto-redeem policy. Redemption
// is never exposed to the user: the wallet decides when to materialize
// available credits back into a vTXO.
type AutoRedeemConfig struct {
	// Enabled turns the periodic auto-redeem sweep on.
	Enabled bool

	// MinRedeemSat is the available-credit threshold above which a sweep
	// redeems. Zero defaults to the operator dust limit at runtime, the
	// smallest amount that can legally become a vTXO.
	MinRedeemSat uint64

	// Interval is the period between idle watermark sweeps.
	Interval time.Duration

	// EarmarkedSat, when set, reports the credit balance reserved by
	// in-flight wallet operations that have not yet written a durable
	// credit_operations row — chiefly a credit-backed PrepareSend whose row
	// is created only at Send. The sweep subtracts this from available
	// credits before deciding, so an idle watermark sweep never redeems
	// credits the user is about to spend. Nil disables the earmark
	// interlock (the boot path wires it from the wallet's prepared-send
	// store, often after the registry is constructed, via
	// Registry.SetEarmarkProvider).
	EarmarkedSat EarmarkFunc
}

const (
	// DefaultPollInterval is the default reconciliation poll backoff.
	DefaultPollInterval = 2 * time.Second

	// DefaultAutoRedeemInterval is the default idle watermark sweep period.
	DefaultAutoRedeemInterval = 5 * time.Minute

	// DefaultAdmitTimeout bounds the synchronous server work an admission
	// performs on the supervisor goroutine.
	DefaultAdmitTimeout = 30 * time.Second
)
