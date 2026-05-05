package swaps

import (
	"context"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btclog/v2"
	sdkark "github.com/lightninglabs/darepo-client/sdk/ark"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
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

// SwapSummary is the stable list view for one persisted swap session.
type SwapSummary struct {
	// Direction identifies whether this is a pay or receive session.
	Direction SwapDirection

	// PaymentHash is the Lightning payment hash for the swap.
	PaymentHash lntypes.Hash

	// State is the current durable FSM state.
	State string

	// Pending is true when the session can still be resumed.
	Pending bool

	// AmountSat is the quoted or requested swap amount in satoshis.
	AmountSat int64

	// FeeSat is the negotiated swap-server fee in satoshis when known.
	FeeSat uint64

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
	CreateInvoice(ctx context.Context,
		amountSat btcutil.Amount, memo string,
		routeHint *RouteHint, expiry time.Duration,
		preimage *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash,
		error)

	// CreateInvoiceWithKey builds one signed invoice using the client's
	// receive auth key. Receive swaps use this key as the invoice
	// destination and later decode the forwarded final-hop onion with it.
	CreateInvoiceWithKey(ctx context.Context,
		amountSat btcutil.Amount, memo string,
		routeHint *RouteHint, expiry time.Duration,
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

	// IncomingExpiryHeight is the CLTV expiry LND reported to the server.
	IncomingExpiryHeight uint32

	// ChannelID is the virtual channel ID used in the invoice route hint.
	ChannelID uint64

	// OnionBlob is the raw final-hop onion blob forwarded by the server.
	OnionBlob []byte

	// VHTLCConfig contains the script parameters for the funded vHTLC.
	VHTLCConfig VHTLCConfig

	// VHTLCOutpoint is the funded outpoint when known by the server.
	VHTLCOutpoint string

	// VHTLCAmountSat is the indexed funded amount when known by the server.
	VHTLCAmountSat int64
}

// OutSwapHtlcNotification carries one mailbox-delivered out-swap HTLC event
// and an optional acknowledgement hook.
type OutSwapHtlcNotification struct {
	Event *OutSwapHtlcEvent
	Ack   func(context.Context) error
}

// OutSwapEventReceiver waits for server-pushed out-swap mailbox events.
type OutSwapEventReceiver interface {
	WaitOutSwapHtlc(
		ctx context.Context,
		paymentHash lntypes.Hash,
		mailboxPubkey *btcec.PublicKey,
	) (*OutSwapHtlcNotification, error)
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

	// ServerPubkey is the swap server's public key for this swap
	// instance.
	ServerPubkey *btcec.PublicKey

	// VHTLCConfig contains the virtual HTLC parameters negotiated
	// for this swap.
	VHTLCConfig VHTLCConfig

	// Expiry is the wall-clock deadline by which the swap must
	// complete before it is considered expired.
	Expiry time.Time
}

// SwapServerConn abstracts the connection to the swap server's
// gRPC service. This allows the client to talk to the swap server
// without importing the server module.
type SwapServerConn interface {
	// RequestChannelID asks the server for a route hint for this swap.
	RequestChannelID(
		ctx context.Context,
		vhtlcPubkey *btcec.PublicKey,
		paymentHash lntypes.Hash,
		expirySeconds uint32,
	) (*RouteHint, error)

	// CreateInSwap initiates an Ark->LN swap on the server.
	CreateInSwap(
		ctx context.Context,
		invoice string,
		maxFeeSat uint64,
		clientVhtlcPubkey *btcec.PublicKey,
	) (*InSwapConfig, error)

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

	// SendOORWithPolicy sends an OOR transfer to a semantic policy-backed
	// destination.
	SendOORWithPolicy(
		ctx context.Context, amountSat int64,
		recipientPolicyTemplate []byte,
	) (string, error)

	// SendOORWithCustomInputs sends an OOR with custom inputs into one
	// standard pubkey-backed Ark receive destination.
	SendOORWithCustomInputs(
		ctx context.Context,
		recipientPubKey []byte,
		amountSat int64,
		inputs []CustomInput,
	) (string, error)

	// IdentityPubKey returns the client's identity pubkey.
	IdentityPubKey(
		ctx context.Context,
	) (*btcec.PublicKey, error)

	// OperatorPubKey returns the Ark operator's pubkey.
	OperatorPubKey(
		ctx context.Context,
	) (*btcec.PublicKey, error)

	// ListLiveVTXOs returns all live VTXOs.
	ListLiveVTXOs(
		ctx context.Context,
	) ([]VTXOInfo, error)

	// ListSpentVTXOs returns all locally known spent VTXOs.
	ListSpentVTXOs(
		ctx context.Context,
	) ([]VTXOInfo, error)

	// FindLiveVTXOByPkScript returns the live VTXO matching the given
	// script when one is visible on the authoritative indexer.
	FindLiveVTXOByPkScript(
		ctx context.Context, pkScript []byte,
	) (*VTXOInfo, error)

	// FindSpentVTXOByPkScript returns the spent VTXO matching the given
	// script when one is visible on the authoritative indexer.
	FindSpentVTXOByPkScript(
		ctx context.Context, pkScript []byte,
	) (*VTXOInfo, error)

	// GetIndexedOORSession returns the indexed Ark package plus
	// finalized checkpoints for one deterministic OOR session.
	GetIndexedOORSession(
		ctx context.Context, pkScript []byte, sessionTxID string,
	) (*OORPackageInfo, error)

	// AllocateReceiveScript allocates a fresh wallet-owned receive
	// destination.
	AllocateReceiveScript(
		ctx context.Context, label string,
	) (*ReceiveInfo, error)
}

// CustomInput aliases the Ark SDK's typed custom OOR input.
type CustomInput = sdkark.CustomOORInput

// VTXOInfo aliases the Ark SDK's typed VTXO metadata.
type VTXOInfo = sdkark.VTXOInfo

// OORPackageInfo aliases the Ark SDK's typed indexed OOR session view.
type OORPackageInfo = sdkark.IndexedOORSessionInfo

// SwapClient is the high-level client API for Lightning<->Ark
// swaps.
type SwapClient struct {
	server     SwapServerConn
	daemon     DaemonConn
	invoiceGen InvoiceCreator
	outEvents  OutSwapEventReceiver
	store      *Store
	log        btclog.Logger

	receiveAuthMu     sync.Mutex
	receiveAuthKeyVal ReceiveAuthKey

	waitPollInterval         time.Duration
	waitVHTLCTimeout         time.Duration
	fundingResumeGracePeriod time.Duration
	claimResumeGracePeriod   time.Duration
	fundingExpiryBuffer      time.Duration
	refundLocktimeBuffer     uint32
	claimRetryDelay          time.Duration
	claimMaxAttempts         int
	decodeOutSwapOnion       outSwapOnionDecoder
	now                      func() time.Time
}

// SetOutSwapEventReceiver sets the mailbox event receiver used by
// ReceiveViaLightning. Callers should configure this before starting receives.
func (c *SwapClient) SetOutSwapEventReceiver(
	receiver OutSwapEventReceiver) {

	c.outEvents = receiver
}

// SetReceiveAuthKey sets the client-level receive auth key used for new
// Lightning-to-Ark receive invoices. Callers should configure this before
// starting or resuming receive swaps so mailbox and onion validation use the
// same key that signed the invoice.
func (c *SwapClient) SetReceiveAuthKey(key ReceiveAuthKey) {
	c.receiveAuthMu.Lock()
	defer c.receiveAuthMu.Unlock()

	c.receiveAuthKeyVal = key
}

// NewSwapClient creates a new swap client. The invoice creator may be nil for
// pay-only callers, but ReceiveViaLightning requires one.
func NewSwapClient(server SwapServerConn, daemon DaemonConn,
	log btclog.Logger,
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
		waitVHTLCTimeout:         60 * time.Second,
		fundingResumeGracePeriod: defaultFundingResumeGracePeriod,
		claimResumeGracePeriod:   defaultClaimResumeGracePeriod,
		fundingExpiryBuffer:      defaultFundingExpiryBuffer,
		refundLocktimeBuffer:     defaultRefundLocktimeBuffer,
		claimRetryDelay:          time.Second,
		claimMaxAttempts:         10,
		decodeOutSwapOnion:       decodeOutSwapOnion,
		now:                      time.Now,
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
