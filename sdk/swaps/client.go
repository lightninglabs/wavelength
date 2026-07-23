package swaps

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	sdkark "github.com/lightninglabs/wavelength/sdk/ark"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

// SwapDirection identifies which Lightning/Ark direction one persisted swap
// session belongs to.
type SwapDirection string

const (
	// SwapDirectionPay identifies an Ark-to-Lightning pay session created
	// by PayViaLightning.
	SwapDirectionPay SwapDirection = "pay"

	// SwapDirectionReceive identifies a Lightning-to-Ark receive session
	// created by ReceiveViaLightning.
	SwapDirectionReceive SwapDirection = "receive"
)

// SettlementType identifies the settlement path selected by the swap server.
type SettlementType string

const (
	// SettlementTypeLightning means the swap server bridges through
	// Lightning.
	SettlementTypeLightning SettlementType = "lightning"

	// SettlementTypeInArk means the sender and receiver settle with one
	// vHTLC inside the same Ark instance.
	SettlementTypeInArk SettlementType = "in_ark"

	// SettlementTypeCredit means the swap server pays Lightning from a
	// reserved credit balance without a client-funded vHTLC.
	SettlementTypeCredit SettlementType = "credit"

	// SettlementTypeMixed means the invoice is funded by both a vHTLC and
	// a reserved credit balance.
	SettlementTypeMixed SettlementType = "mixed"
)

// CreditQuote describes how a pay quote uses wallet credits.
type CreditQuote struct {
	MustUseCredit      bool
	CreditAppliedSat   uint64
	CreditShortfallSat uint64
	CreditTopupSat     uint64
	ArkFundingSat      uint64
}

// CreditFundingSource identifies how value enters a credit account.
type CreditFundingSource string

const (
	// CreditFundingLightningReceive means the server creates and owns the
	// Lightning invoice, then credits the wallet account after settlement.
	CreditFundingLightningReceive CreditFundingSource = "lightning_receive"

	// CreditFundingArkTopUp means the server returns a pubkey-backed Ark
	// destination and credits the account after the OOR top-up is visible.
	CreditFundingArkTopUp CreditFundingSource = "ark_topup"
)

// CreditOperationType identifies the durable credit operation family.
type CreditOperationType string

const (
	CreditOperationFunding    CreditOperationType = "funding"
	CreditOperationPay        CreditOperationType = "pay"
	CreditOperationRedemption CreditOperationType = "redemption"
	CreditOperationReceive    CreditOperationType = "receive"
)

// CreditOperationState is the externally visible credit state-machine state.
type CreditOperationState string

const (
	CreditStateCreated         CreditOperationState = "created"
	CreditStateAwaitingPayment CreditOperationState = "awaiting_payment"
	CreditStateCredited        CreditOperationState = "credited"
	CreditStateReserved        CreditOperationState = "reserved"
	CreditStatePayingLightning CreditOperationState = "paying_lightning"
	CreditStateDebited         CreditOperationState = "debited"
	CreditStateSendingOOR      CreditOperationState = "sending_oor"
	CreditStateRedeemed        CreditOperationState = "redeemed"
	CreditStateReleased        CreditOperationState = "released"
	CreditStateExpired         CreditOperationState = "expired"
	CreditStateFailed          CreditOperationState = "failed"
)

// CreateCreditRequest describes one caller intent to materialize credits.
type CreateCreditRequest struct {
	IdempotencyKey string
	Source         CreditFundingSource
	AmountSat      uint64
	Memo           string
}

// RedeemCreditRequest describes one caller intent to materialize credits back
// into a dust-clearing Ark output.
type RedeemCreditRequest struct {
	IdempotencyKey    string
	AmountSat         uint64
	DestinationPubKey []byte
}

// CreditOperation is one server-authoritative credit state machine.
type CreditOperation struct {
	OperationID    string
	Type           CreditOperationType
	State          CreditOperationState
	AmountSat      uint64
	PaymentHash    *lntypes.Hash
	Invoice        string
	DestinationKey []byte
	SessionID      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
	ExpiresAt      *time.Time
	LastError      string
}

// CreditLedgerEntry records one immutable finalized credit or debit.
type CreditLedgerEntry struct {
	EntryID     string
	OperationID string
	Direction   string
	AmountSat   uint64
	CreatedAt   time.Time
}

// CreditSnapshot is the server-authoritative account view.
type CreditSnapshot struct {
	FinalizedSat  uint64
	ReservedSat   uint64
	AvailableSat  uint64
	Operations    []CreditOperation
	LedgerEntries []CreditLedgerEntry
}

// CreditRedemption records the result of a successful credit redemption.
type CreditRedemption struct {
	Operation   CreditOperation
	DebitedSat  uint64
	RedeemedSat uint64
	SessionID   string
}

// OORSendResult contains daemon metadata for an accepted OOR transfer.
type OORSendResult = sdkark.OORSendResult

// SwapSummary is the stable list view for one persisted swap session.
type SwapSummary struct {
	// Direction identifies whether this is a pay or receive session.
	Direction SwapDirection

	// PaymentHash is the Lightning payment hash for the swap.
	PaymentHash lntypes.Hash

	// Preimage is the Lightning payment preimage once the swap revealed it.
	// For a completed pay swap this is the proof of payment for the paid
	// invoice; it is nil until the preimage is durably known.
	Preimage *lntypes.Preimage

	// Invoice is the BOLT-11 invoice associated with the swap.
	Invoice string

	// State is the current durable FSM state.
	State string

	// Pending is true when the session can still be resumed.
	Pending bool

	// AmountSat is the quoted or requested swap amount in satoshis.
	AmountSat int64

	// FeeSat is the negotiated swap-server fee in satoshis when known.
	FeeSat uint64

	// PayerFeeMsat is the payer-paid Lightning route fee quoted for
	// receive swaps. It is not deducted from AmountSat.
	PayerFeeMsat uint64

	// MaxFeeSat is the caller-provided maximum routing fee for pay swaps.
	MaxFeeSat uint64

	// VHTLCOutpoint is the observed Ark vHTLC outpoint, when known.
	VHTLCOutpoint string

	// VHTLCAmountSat is the observed Ark vHTLC amount, when known.
	VHTLCAmountSat int64

	// FundingSessionID is the OOR session that funded a pay swap vHTLC.
	FundingSessionID string

	// ClaimSessionID is the OOR session that claimed a receive swap vHTLC.
	ClaimSessionID string

	// RefundSessionID is the OOR session that refunded a pay swap vHTLC,
	// or the observed spender txid when the refund was adopted from the
	// indexer during resume.
	RefundSessionID string

	// SettlementType identifies whether the swap settles through Lightning
	// or as a same-Ark payment when that detail is durably known.
	SettlementType SettlementType

	// CreditQuote records the credit component of a pay quote when the
	// server selected a credit or mixed rail.
	CreditQuote *CreditQuote

	// RequestedAmountSat is the invoice amount for receive swaps when it
	// can differ from the funded vHTLC amount.
	RequestedAmountSat uint64

	// AttachedCreditSat is the credit amount attached to a credit-assisted
	// receive.
	AttachedCreditSat uint64

	// AvailableCreditSat is the balance considered when the receive route
	// was planned.
	AvailableCreditSat uint64

	// DustLimitSat is the vHTLC dust limit used for receive planning.
	DustLimitSat uint64

	// SenderPubkey is the vHTLC sender key when that remote party is
	// durably known. For same-Ark receives this identifies the paying
	// client; for Lightning-backed receives this is the swap server key.
	SenderPubkey *btcec.PublicKey

	// TerminalReason is the durable reason for failed or intervention
	// terminal states.
	TerminalReason string

	// CreatedAt is the local creation timestamp for the persisted session.
	CreatedAt time.Time

	// UpdatedAt is the last local persistence timestamp for the session.
	UpdatedAt time.Time

	// Deadline is the wall-clock expiry or deadline for the swap.
	Deadline time.Time

	// RefundLocktime is the Ark block height where the refund path matures.
	RefundLocktime uint32
}

// ReceiveResult holds the outcome of a successful
// ReceiveViaLightning call.
type ReceiveResult struct {
	// Invoice is the encoded BOLT-11 payment request that the
	// payer should pay.
	Invoice string

	// Preimage is the preimage used to construct the vHTLC.
	Preimage lntypes.Preimage

	// PaymentHash is the SHA-256 hash of the preimage.
	PaymentHash lntypes.Hash

	// VTXOOutpoint is the outpoint of the claimed VTXO in
	// "txid:vout" format.
	VTXOOutpoint string

	// AmountSat is the value of the claimed VTXO in satoshis.
	AmountSat int64
}

// ReceiveVHTLCInfo holds the script details for one prepared
// Lightning-to-Ark receive session.
type ReceiveVHTLCInfo struct {
	// PkScript is the taproot output script for the expected vHTLC.
	PkScript []byte

	// ClaimScript is the tapscript leaf used to sweep the funded vHTLC
	// with the preimage.
	ClaimScript []byte
}

// ReceiveInfo aliases the Ark SDK's typed wallet-owned receive destination.
type ReceiveInfo = sdkark.ReceiveInfo

// PayResult holds the outcome of a successful PayViaLightning call.
type PayResult struct {
	// PaymentHash is the SHA-256 payment hash of the paid
	// invoice.
	PaymentHash lntypes.Hash

	// Preimage is the preimage used by the server to claim the funded
	// vHTLC after paying the Lightning invoice.
	Preimage lntypes.Preimage

	// FundingSessionID is the OOR session identifier returned by the daemon
	// when the vHTLC funding transfer is submitted.
	FundingSessionID string

	// FeeSat is the fee in satoshis charged by the swap server.
	FeeSat uint64
}

// InvoiceCreator creates signed Lightning invoices for out-swaps.
type InvoiceCreator interface {
	// CreateInvoice builds one signed invoice using the provided route
	// hint and optional fixed preimage.
	CreateInvoice(ctx context.Context, amountSat btcutil.Amount,
		memo string, routeHint *RouteHint, expiry time.Duration,
		preimage *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash,
		error)

	// CreateInvoiceWithKey builds one signed invoice using the client's
	// receive auth key. Receive swaps use this key as the invoice
	// destination and later decode the forwarded final-hop onion with it.
	CreateInvoiceWithKey(ctx context.Context, amountSat btcutil.Amount,
		memo string, routeHint *RouteHint, expiry time.Duration,
		authKey keychain.SingleKeyMessageSigner,
		preimage *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash,
		error)

	// CreateInvoiceWithKeyRouteHintPaths builds one signed invoice with
	// the client's receive auth key and one BOLT-11 "r" field per
	// alternative route-hint path from the server. Multi-backend swap
	// servers return one path per backend so the sender can route
	// through any of them.
	CreateInvoiceWithKeyRouteHintPaths(ctx context.Context,
		amountSat btcutil.Amount, memo string,
		routeHintPaths [][]*RouteHint, expiry time.Duration,
		authKey keychain.SingleKeyMessageSigner,
		preimage *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash,
		error)
}

// RouteHint describes a single hop hint for Lightning invoices.
type RouteHint struct {
	// NodeID is the compressed public key of the hop's Lightning
	// node.
	NodeID []byte

	// ChannelID is the short channel ID for this hop.
	ChannelID uint64

	// FeeBaseMsat is the base fee in milli-satoshis charged by
	// this hop.
	FeeBaseMsat uint64

	// FeePropPpm is the proportional fee rate in parts-per-million
	// charged by this hop.
	FeePropPpm uint64

	// CltvExpiryDelta is the CLTV expiry delta required by this
	// hop.
	CltvExpiryDelta uint32
}

// OutSwapQuote is the complete server quote for one Lightning-to-Ark receive
// route.
type OutSwapQuote struct {
	// RouteHintPaths is every alternative private route-hint path for
	// the receive, one per swap-server backend. The final hop of every
	// path is the swap server's virtual channel. The SDK embeds one
	// BOLT-11 "r" field per path so the sender's pathfinding can pick
	// whichever backend is reachable.
	RouteHintPaths [][]*RouteHint

	// ReceiveAmountSat is the exact Ark amount the receiver expects.
	ReceiveAmountSat btcutil.Amount

	// PayerFeeMsat is the payer-paid route fee quoted by the swap server.
	PayerFeeMsat uint64

	// RequestedAmountSat is the invoice amount requested by the receiver.
	RequestedAmountSat uint64

	// AvailableCreditSat is the server-authoritative credit balance
	// considered when the receive route was planned.
	AvailableCreditSat uint64

	// AttachedCreditSat is the credit amount reserved and added to the
	// funded vHTLC.
	AttachedCreditSat uint64

	// VHTLCAmountSat is the funded vHTLC amount the client should expect.
	VHTLCAmountSat uint64

	// DustLimitSat is the minimum vHTLC output amount used by the server.
	DustLimitSat uint64

	// SettlementType identifies the receive rail selected by the server.
	SettlementType SettlementType
}

// VHTLCConfig holds the timelocks and keys for a vHTLC.
type VHTLCConfig struct {
	// RefundLocktime is the absolute block height after which the
	// sender can reclaim funds via the refund path.
	RefundLocktime uint32

	// UnilateralClaimDelay is the relative delay in blocks before
	// the receiver can claim a unilateral exit.
	UnilateralClaimDelay uint32

	// UnilateralRefundDelay is the relative delay in blocks
	// before the sender can refund a unilateral exit.
	UnilateralRefundDelay uint32

	// UnilateralRefundWithoutReceiverDelay is the relative delay
	// in blocks before the sender can refund without receiver
	// cooperation.
	UnilateralRefundWithoutReceiverDelay uint32

	// SwapServerPubkey is the swap server's public key used in
	// the vHTLC tapscript spend paths.
	SwapServerPubkey []byte
}

// OutSwapHtlcEvent carries the server-funded HTLC metadata that the client
// validates before revealing its invoice preimage.
type OutSwapHtlcEvent struct {
	// PaymentHash is the intercepted Lightning payment hash.
	PaymentHash lntypes.Hash

	// AmountSat is the amount funded by the server.
	AmountSat int64

	// RequestedAmountSat is the invoice amount that the Lightning HTLCs
	// pay.
	RequestedAmountSat uint64

	// AttachedCreditSat is the credit amount attached to this vHTLC.
	AttachedCreditSat uint64

	// OnionBlob is the raw final-hop onion blob forwarded by the server.
	OnionBlob []byte

	// VHTLCConfig contains the script parameters for the funded vHTLC.
	VHTLCConfig VHTLCConfig

	// Parts lists the individual HTLC shards of a multi-part payment set.
	// When empty the event is a legacy single-part payment carried by
	// OnionBlob.
	Parts []OutSwapHtlcPart
}

// OutSwapHtlcPart carries one HTLC shard of a multi-part out-swap payment
// set. Each shard has its own final-hop onion that the client validates
// before acknowledging the event.
type OutSwapHtlcPart struct {
	// AmountMsat is the millisatoshi amount forwarded by this shard.
	AmountMsat lnwire.MilliSatoshi

	// OnionBlob is the raw final-hop onion blob forwarded by the server
	// for this shard.
	OnionBlob []byte
}

// OutSwapHtlcNotification carries one mailbox-delivered out-swap HTLC event
// and an optional acknowledgement hook.
type OutSwapHtlcNotification struct {
	Event     *OutSwapHtlcEvent
	AckCursor uint64
	Ack       func(context.Context) error
}

// InArkHtlcEvent carries same-Ark vHTLC metadata that the receiver validates
// before revealing its invoice preimage.
type InArkHtlcEvent struct {
	// PaymentHash is the invoice payment hash.
	PaymentHash lntypes.Hash

	// AmountSat is the amount funded by the sender.
	AmountSat int64

	// SenderPubkey is the sender key used in the vHTLC refund paths.
	SenderPubkey *btcec.PublicKey

	// VHTLCConfig contains the script parameters for the funded vHTLC.
	VHTLCConfig VHTLCConfig

	// VHTLCOutpoint is the funded outpoint when known by the server.
	VHTLCOutpoint string

	// VHTLCAmountSat is the indexed funded amount when known by the server.
	VHTLCAmountSat int64

	// RequestedAmountSat is the invoice amount for a credit-shaped event.
	// When set together with AttachedCreditSat, AmountSat carries the
	// padded vHTLC amount of a credit-attach receive plan and the funding
	// sender is the swap server. Zero on legacy direct p2p events.
	RequestedAmountSat uint64

	// AttachedCreditSat is the reserved credit amount added to the vHTLC
	// on top of RequestedAmountSat.
	AttachedCreditSat uint64
}

// IncomingVHTLCNotification carries either a Lightning-backed out-swap event
// or a direct same-Ark vHTLC event.
type IncomingVHTLCNotification struct {
	OutSwap   *OutSwapHtlcEvent
	InArk     *InArkHtlcEvent
	AckCursor uint64
	Ack       func(context.Context) error
}

// ForfeitSignaturePayload is the exact transcript that one vHTLC refresh
// participant signs. The unsigned forfeit transaction and connector metadata
// are included so the signer can bind its signature to one concrete round
// assignment instead of a reusable high-level refresh intent.
type ForfeitSignaturePayload struct {
	// RequestID uniquely identifies this signing request.
	RequestID []byte

	// PaymentHash identifies the swap whose vHTLC is being refreshed.
	PaymentHash lntypes.Hash

	// VHTLCOutpoint is the vHTLC input being forfeited into the refresh.
	VHTLCOutpoint string

	// VHTLCAmountSat is the value of VHTLCOutpoint in satoshis.
	VHTLCAmountSat int64

	// VHTLCPkScript is the scriptPubKey of the vHTLC input.
	VHTLCPkScript []byte

	// VHTLCPolicyTemplate is the semantic vHTLC policy template.
	VHTLCPolicyTemplate []byte

	// ForfeitSpendPath is the tapscript path used for the forfeit input.
	ForfeitSpendPath []byte

	// UnsignedForfeitTx is the exact unsigned forfeit transaction to sign.
	UnsignedForfeitTx []byte

	// ConnectorOutpoint is the connector input assigned by the round.
	ConnectorOutpoint string

	// ConnectorAmountSat is the value of ConnectorOutpoint in satoshis.
	ConnectorAmountSat int64

	// ConnectorPkScript is the scriptPubKey of the connector input.
	ConnectorPkScript []byte

	// ServerForfeitPkScript is the output script paid by the forfeit tx.
	ServerForfeitPkScript []byte
}

// ForfeitParticipantSignature carries one participant signature for the exact
// forfeit transaction described by ForfeitSignaturePayload.
type ForfeitParticipantSignature struct {
	PubKey    []byte
	Signature []byte
}

// OutSwapForfeitSignatureNotification carries a mailbox-delivered request for
// the receiver's participant signature on one out-swap vHTLC refresh.
type OutSwapForfeitSignatureNotification struct {
	Payload   *ForfeitSignaturePayload
	AckCursor uint64
	Ack       func(context.Context) error
}

// OutSwapEventReceiver waits for server-pushed out-swap mailbox events.
type OutSwapEventReceiver interface {
	WaitOutSwapHtlc(ctx context.Context, paymentHash lntypes.Hash,
		mailboxPubkey *btcec.PublicKey) (
		*OutSwapHtlcNotification,
		error,
	)

	AckOutSwapHtlc(
		ctx context.Context,
		paymentHash lntypes.Hash,
		mailboxPubkey *btcec.PublicKey,
		cursor uint64,
	) error
}

// OutSwapForfeitSignatureReceiver waits for server-pushed out-swap refresh
// signing requests.
type OutSwapForfeitSignatureReceiver interface {
	WaitOutSwapForfeitSignature(ctx context.Context,
		paymentHash lntypes.Hash,
		mailboxPubkey *btcec.PublicKey) (
		*OutSwapForfeitSignatureNotification, error)
}

// IncomingVHTLCEventReceiver waits for any incoming vHTLC event type that can
// satisfy a prepared receive invoice.
type IncomingVHTLCEventReceiver interface {
	WaitIncomingVHTLC(ctx context.Context, paymentHash lntypes.Hash,
		mailboxPubkey *btcec.PublicKey) (
		*IncomingVHTLCNotification,
		error,
	)
}

// InSwapConfig is returned by the server when creating an in-swap.
type InSwapConfig struct {
	// PaymentHash is the SHA-256 payment hash extracted from the
	// submitted invoice.
	PaymentHash lntypes.Hash

	// AmountSat is the total amount in satoshis locked in the
	// swap.
	AmountSat int64

	// FeeSat is the fee in satoshis charged by the swap server
	// for this swap.
	FeeSat uint64

	// ServerFeeSat is the service fee retained by the swap server.
	ServerFeeSat uint64

	// RoutingFeeBudgetSat is the client-funded Lightning routing
	// allowance included in FeeSat.
	RoutingFeeBudgetSat uint64

	// ServerPubkey is the swap server's public key for this swap
	// instance.
	ServerPubkey *btcec.PublicKey

	// VHTLCConfig contains the virtual HTLC parameters negotiated
	// for this swap.
	VHTLCConfig VHTLCConfig

	// Expiry is the wall-clock deadline by which the swap must
	// complete before it is considered expired.
	Expiry time.Time

	// SettlementType identifies whether this pay session is bridged through
	// Lightning or settled directly inside Ark.
	SettlementType SettlementType

	// CreditQuote records the credit reservation used by credit or mixed
	// pays.
	CreditQuote *CreditQuote

	// Preimage is set for credit-only pays that complete inside the server
	// CreateInSwap call without creating a vHTLC.
	Preimage *lntypes.Preimage
}

// InSwapQuote previews an Ark-to-Lightning payment without creating durable
// swap state on either side.
type InSwapQuote struct {
	// PaymentHash is the SHA-256 payment hash extracted from the
	// submitted invoice.
	PaymentHash lntypes.Hash

	// InvoiceAmountSat is the BOLT-11 destination amount.
	InvoiceAmountSat uint64

	// AmountSat is the total amount in satoshis the wallet would lock in
	// the vHTLC.
	AmountSat uint64

	// FeeSat is the fee in satoshis charged by the swap server.
	FeeSat uint64

	// ServerFeeSat is the service fee retained by the swap server.
	ServerFeeSat uint64

	// EstimatedRoutingFeeSat is the server's current whole-satoshi route
	// estimate.
	EstimatedRoutingFeeSat uint64

	// RoutingFeeBudgetSat is the Lightning routing allowance that would be
	// included in FeeSat.
	RoutingFeeBudgetSat uint64

	// Expiry is the wall-clock deadline by which the quoted swap must
	// complete before it is considered stale.
	Expiry time.Time

	// SettlementType identifies whether the payment would bridge through
	// Lightning or settle directly inside Ark.
	SettlementType SettlementType

	// ExceedsMaxFee is true when the caller supplied a max fee and the
	// quoted fee is larger than that cap.
	ExceedsMaxFee bool

	// CreditQuote describes how credits would be used for this invoice.
	CreditQuote *CreditQuote
}

// SwapServerConn abstracts the connection to the swap server's
// gRPC service. This allows the client to talk to the swap server
// without importing the server module.
type SwapServerConn interface {
	// RequestChannelID asks the server for a route hint for this swap.
	// supportsInArkCredit advertises whether the wired event receiver can
	// consume credit-shaped in-ark HTLC events; the server only routes a
	// credit-attach receive through the in-ark leg when it is set.
	RequestChannelID(ctx context.Context, vhtlcPubkey *btcec.PublicKey,
		paymentHash lntypes.Hash, amountSat btcutil.Amount,
		expirySeconds uint32,
		supportsInArkCredit bool) (*OutSwapQuote, error)

	// AcknowledgeOutSwapHTLC tells the server this receiver validated and
	// durably accepted the out-swap HTLC event.
	AcknowledgeOutSwapHTLC(ctx context.Context, paymentHash lntypes.Hash,
		vhtlcPubkey *btcec.PublicKey) error

	// CreateInSwap initiates an Ark->LN swap on the server.
	CreateInSwap(ctx context.Context, invoice string, maxFeeSat uint64,
		clientVhtlcPubkey *btcec.PublicKey) (*InSwapConfig, error)

	// QuoteInSwap previews an Ark->LN swap without creating server or
	// client state.
	QuoteInSwap(ctx context.Context, invoice string,
		maxFeeSat uint64) (*InSwapQuote, error)

	// AuthorizeInSwapRefund asks the swap server to sign one exact
	// cooperative refund spend after it has safely failed the Lightning
	// payment.
	AuthorizeInSwapRefund(ctx context.Context, paymentHash lntypes.Hash,
		vhtlcOutpoint string, vhtlcAmountSat int64, vhtlcPolicyTemplate,
		refundSpendPath,
		checkpointPSBT []byte) (*InSwapRefundAuthorization, error)

	// SignInSwapForfeit asks the swap server to sign its participant share
	// for one exact in-swap vHTLC refresh forfeit transaction.
	SignInSwapForfeit(ctx context.Context,
		payload *ForfeitSignaturePayload) (
		*ForfeitParticipantSignature,
		error,
	)

	// SubmitOutSwapForfeitSignature submits this receiver's participant
	// signature for one mailbox-delivered out-swap vHTLC refresh request.
	SubmitOutSwapForfeitSignature(ctx context.Context,
		payload *ForfeitSignaturePayload,
		signature *ForfeitParticipantSignature) error

	// Close closes the connection.
	Close() error
}

// DaemonConn abstracts the connection to the client's own daemon for wallet
// operations such as OOR sends and indexed VTXO lookups.
//
// The swap FSMs intentionally depend on this full daemon capability
// boundary.
//
//nolint:interfacebloat
type DaemonConn interface {
	// BlockHeight returns the daemon's best known chain height.
	BlockHeight(ctx context.Context) (uint32, error)

	// SendOORWithPolicyDetails sends an OOR transfer to a semantic
	// policy-backed destination.
	SendOORWithPolicyDetails(ctx context.Context, amountSat int64,
		recipientPolicyTemplate []byte) (*OORSendResult, error)

	// SendOORWithCustomInputs sends an OOR with custom inputs into one
	// standard pubkey-backed Ark receive destination.
	SendOORWithCustomInputs(ctx context.Context, recipientPubKey []byte,
		amountSat int64, inputs []CustomInput) (string, error)

	// ArmVHTLCRecovery stores a dormant daemon-owned on-chain recovery job.
	ArmVHTLCRecovery(ctx context.Context,
		req *waverpc.ArmVHTLCRecoveryRequest) (
		*waverpc.ArmVHTLCRecoveryResponse, error)

	// EscalateVHTLCRecovery starts or resumes the unroll path for an armed
	// recovery job.
	EscalateVHTLCRecovery(ctx context.Context,
		req *waverpc.EscalateVHTLCRecoveryRequest) (
		*waverpc.EscalateVHTLCRecoveryResponse, error)

	// CancelVHTLCRecovery records that cooperative settlement won before
	// the armed recovery path was needed.
	CancelVHTLCRecovery(ctx context.Context,
		req *waverpc.CancelVHTLCRecoveryRequest) (
		*waverpc.CancelVHTLCRecoveryResponse, error)

	// GetVHTLCRecoveryStatus returns the daemon's durable recovery row and
	// current unroll status, when present.
	GetVHTLCRecoveryStatus(ctx context.Context,
		req *waverpc.GetVHTLCRecoveryStatusRequest) (
		*waverpc.GetVHTLCRecoveryStatusResponse, error)

	// PrepareOORWithCustomInputs builds a deterministic custom-input OOR
	// package without submitting it.
	PrepareOORWithCustomInputs(ctx context.Context, recipientPubKey []byte,
		amountSat int64, inputs []CustomInput) (*PreparedOOR, error)

	// IdentityPubKey returns the client's identity pubkey.
	IdentityPubKey(ctx context.Context) (*btcec.PublicKey, error)

	// OperatorPubKey returns the Ark operator's pubkey.
	OperatorPubKey(ctx context.Context) (*btcec.PublicKey, error)

	// ListLiveVTXOs returns all live VTXOs.
	ListLiveVTXOs(ctx context.Context) ([]VTXOInfo, error)

	// ListSpentVTXOs returns all locally known spent VTXOs.
	ListSpentVTXOs(ctx context.Context) ([]VTXOInfo, error)

	// FindLiveVTXOByPkScript returns the live VTXO matching the given
	// script when one is visible on the authoritative indexer.
	FindLiveVTXOByPkScript(ctx context.Context,
		pkScript []byte) (*VTXOInfo, error)

	// FindSpentVTXOByPkScript returns the spent VTXO matching the given
	// script when one is visible on the authoritative indexer.
	FindSpentVTXOByPkScript(ctx context.Context,
		pkScript []byte) (*VTXOInfo, error)

	// GetIndexedOORSession returns the indexed Ark package plus
	// finalized checkpoints for one deterministic OOR session.
	GetIndexedOORSession(ctx context.Context, pkScript []byte,
		sessionTxID string) (*OORPackageInfo, error)

	// GetOORSession returns the daemon's local durable status for one OOR
	// session.
	GetOORSession(ctx context.Context,
		sessionID string) (*waverpc.OORSessionInfo, error)

	// AllocateReceiveScript allocates a fresh wallet-owned receive
	// destination.
	AllocateReceiveScript(ctx context.Context,
		label string) (*ReceiveInfo, error)

	// ReceiveAuthKey returns the payment-scoped receive-auth public key.
	ReceiveAuthKey(ctx context.Context,
		paymentHash lntypes.Hash) (*btcec.PublicKey, error)

	// SignReceiveAuthMessage signs one message with the payment-scoped
	// receive-auth key.
	SignReceiveAuthMessage(ctx context.Context, paymentHash lntypes.Hash,
		message []byte, doubleHash bool) (*ecdsa.Signature, error)

	// SignReceiveAuthMessageCompact signs one message with the
	// payment-scoped receive-auth key and returns a compact signature.
	SignReceiveAuthMessageCompact(ctx context.Context,
		paymentHash lntypes.Hash, message []byte,
		doubleHash bool) ([]byte, error)

	// ReceiveAuthECDH derives one Sphinx shared secret with the
	// payment-scoped receive-auth key.
	ReceiveAuthECDH(ctx context.Context, paymentHash lntypes.Hash,
		pubKey *btcec.PublicKey) ([32]byte, error)

	// SignVTXOForfeit signs one exact connector-bound forfeit transaction
	// with the daemon identity key.
	SignVTXOForfeit(ctx context.Context,
		req *waverpc.SignVTXOForfeitRequest) (
		*waverpc.SignVTXOForfeitResponse, error)
}

// CustomInput aliases the Ark SDK's typed custom OOR input.
type CustomInput = sdkark.CustomOORInput

// PreparedOOR aliases the Ark SDK's deterministic prepared OOR view.
type PreparedOOR = sdkark.PreparedOOR

// PreparedOORCustomInput aliases the Ark SDK's prepared custom input view.
type PreparedOORCustomInput = sdkark.PreparedOORCustomInput

// TaprootScriptSignature aliases the Ark SDK's external tapscript signature.
type TaprootScriptSignature = sdkark.TaprootScriptSignature

// VTXOInfo aliases the Ark SDK's typed VTXO metadata.
type VTXOInfo = sdkark.VTXOInfo

// OORPackageInfo aliases the Ark SDK's typed indexed OOR session view.
type OORPackageInfo = sdkark.IndexedOORSessionInfo

// InSwapRefundAuthorization carries the swap server's cooperative refund
// signature and the terminal Lightning failure reason that made it safe.
type InSwapRefundAuthorization struct {
	// Signature is the server's tapscript signature for the prepared
	// refund checkpoint input.
	Signature TaprootScriptSignature

	// FailureReason describes why the swap server considers the Lightning
	// payment terminal and therefore safe to refund immediately.
	FailureReason string
}

// SwapClient is the high-level client API for Lightning<->Ark
// swaps.
type SwapClient struct {
	server     SwapServerConn
	daemon     DaemonConn
	invoiceGen InvoiceCreator
	outEvents  OutSwapEventReceiver
	store      *Store
	log        btclog.Logger

	waitPollInterval         time.Duration
	overdueReceivePollWindow time.Duration
	fundingResumeGracePeriod time.Duration
	claimResumeGracePeriod   time.Duration
	fundingExpiryBuffer      time.Duration
	refundLocktimeBuffer     uint32
	claimRetryDelay          time.Duration
	claimMaxAttempts         int
	recoveryPolicy           RecoveryPolicy
	decodeOutSwapOnion       outSwapOnionDecoder
	chainParams              *chaincfg.Params
	now                      func() time.Time
}

// SetOutSwapEventReceiver sets the mailbox event receiver used by
// ReceiveViaLightning. Callers should configure this before starting receives.
func (c *SwapClient) SetOutSwapEventReceiver(receiver OutSwapEventReceiver) {
	c.outEvents = receiver
}

// SetChainParams sets the Bitcoin network used to decode pay-side
// BOLT-11 invoices. PayViaLightning rejects invoices when no network params are
// configured.
func (c *SwapClient) SetChainParams(chainParams *chaincfg.Params) {
	c.chainParams = chainParams
}

// NewSwapClient creates a new swap client. The invoice creator may be nil for
// pay-only callers, but ReceiveViaLightning requires one.
func NewSwapClient(server SwapServerConn, daemon DaemonConn, log btclog.Logger,
	invoiceGen InvoiceCreator) *SwapClient {

	return NewSwapClientWithStore(
		server, daemon, log, invoiceGen, nil,
	)
}

// NewSwapClientWithStore creates a new swap client and optionally enables
// isolated SQL-backed swap session persistence through store.
func NewSwapClientWithStore(server SwapServerConn, daemon DaemonConn,
	log btclog.Logger, invoiceGen InvoiceCreator,
	store *Store) *SwapClient {

	if log == nil {
		log = btclog.Disabled
	}

	var outEvents OutSwapEventReceiver
	if receiver, ok := server.(OutSwapEventReceiver); ok {
		outEvents = receiver
	}

	return &SwapClient{
		server:                   server,
		daemon:                   daemon,
		invoiceGen:               invoiceGen,
		outEvents:                outEvents,
		store:                    store,
		log:                      log,
		waitPollInterval:         2 * time.Second,
		overdueReceivePollWindow: defaultOverdueReceiveMailboxPollWindow,
		fundingResumeGracePeriod: defaultFundingResumeGracePeriod,
		claimResumeGracePeriod:   defaultClaimResumeGracePeriod,
		fundingExpiryBuffer:      defaultFundingExpiryBuffer,
		refundLocktimeBuffer:     defaultRefundLocktimeBuffer,
		claimRetryDelay:          time.Second,
		claimMaxAttempts:         10,
		recoveryPolicy:           DefaultRecoveryPolicy(),
		decodeOutSwapOnion:       decodeOutSwapOnion,
		chainParams:              invoiceCreatorChainParams(invoiceGen),
		now:                      time.Now,
	}
}

// SetRecoveryPolicy overrides the automatic vHTLC recovery escalation policy.
// Arming remains immediate; this policy only decides when a cooperative
// claim/refund failure should turn into costly on-chain unroll.
func (c *SwapClient) SetRecoveryPolicy(policy RecoveryPolicy) {
	if c == nil {
		return
	}

	c.recoveryPolicy = policy.WithDefaults()
}

// invoiceCreatorChainParams returns the chain params carried by the built-in
// invoice creators. Custom invoice creators can still configure pay invoice
// decoding explicitly through SetChainParams.
func invoiceCreatorChainParams(creator InvoiceCreator) *chaincfg.Params {
	switch c := creator.(type) {
	case *InvoiceGenerator:
		if c == nil {
			return nil
		}

		return c.chainParams

	case *DirectInvoiceCreator:
		if c == nil || c.generator == nil {
			return nil
		}

		return c.generator.chainParams

	default:
		return nil
	}
}

// currentTime returns the clock used by the swap client. Tests can override
// c.now directly because they live in this package, while production callers
// use the wall clock.
func (c *SwapClient) currentTime() time.Time {
	if c == nil || c.now == nil {
		return time.Now()
	}

	return c.now()
}
