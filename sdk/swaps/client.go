package swaps

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
)

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

// PayResult holds the outcome of a successful PayViaLightning call.
type PayResult struct {
	// PaymentHash is the SHA-256 payment hash of the paid
	// invoice.
	PaymentHash lntypes.Hash

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
	CreateInvoice(ctx context.Context, amountSat btcutil.Amount, memo string,
		routeHint *RouteHint, expiry time.Duration,
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
	// RequestChannelID asks the server for a route hint and
	// the locked-in vHTLC configuration for this swap.
	RequestChannelID(
		ctx context.Context,
		vhtlcPubkey *btcec.PublicKey,
		expirySeconds uint32,
	) (*RouteHint, *VHTLCConfig, error)

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

// DaemonConn abstracts the connection to the client's own daemon
// for wallet operations (SendOOR, ListVTXOs, etc.).
type DaemonConn interface {
	// SendOOR sends an OOR transfer to the given pkScript.
	SendOOR(
		ctx context.Context, pkScript []byte, amountSat int64,
	) (string, error)

	// SendOORWithCustomInputs sends an OOR with custom inputs.
	SendOORWithCustomInputs(
		ctx context.Context,
		recipientPkScript []byte,
		amountSat int64,
		inputs []CustomInput,
	) (string, error)

	// GetIdentityPubkey returns the client's identity pubkey.
	GetIdentityPubkey(
		ctx context.Context,
	) (*btcec.PublicKey, error)

	// GetOperatorPubkey returns the Ark operator's pubkey.
	GetOperatorPubkey(
		ctx context.Context,
	) (*btcec.PublicKey, error)

	// ListLiveVTXOs returns all live VTXOs.
	ListLiveVTXOs(
		ctx context.Context,
	) ([]VTXOInfo, error)

	// FindLiveVTXOByPkScript returns the live VTXO matching the given
	// script when one is visible on the authoritative indexer.
	FindLiveVTXOByPkScript(
		ctx context.Context, pkScript []byte,
	) (*VTXOInfo, error)

	// NewOORReceiveScript allocates a fresh OOR receive script.
	NewOORReceiveScript(
		ctx context.Context,
	) ([]byte, error)
}

// CustomInput describes a custom input for OOR transfers.
type CustomInput struct {
	// Outpoint is the outpoint of the custom input in
	// "txid:vout" format.
	Outpoint string

	// AmountSat is the value of the custom input in satoshis.
	AmountSat int64

	// PkScript is the output script of the custom input.
	PkScript []byte

	// SpendWitnessScript is the tapscript leaf script bytes for
	// the spend path.
	SpendWitnessScript []byte

	// SpendControlBlock is the BIP-341 control block for
	// script-path spending.
	SpendControlBlock []byte

	// ConditionWitness holds extra witness elements needed by the
	// spend script beyond signatures (e.g., preimage).
	ConditionWitness [][]byte
}

// VTXOInfo holds basic VTXO metadata.
type VTXOInfo struct {
	// Outpoint is the outpoint of the VTXO in "txid:vout" format.
	Outpoint string

	// AmountSat is the value of the VTXO in satoshis.
	AmountSat int64

	// PkScript is the output script of the VTXO.
	PkScript []byte
}

// SwapClient is the high-level client API for Lightning<->Ark
// swaps.
type SwapClient struct {
	server     SwapServerConn
	daemon     DaemonConn
	invoiceGen InvoiceCreator
	log        btclog.Logger

	waitPollInterval time.Duration
	waitVHTLCTimeout time.Duration
	claimRetryDelay  time.Duration
	claimMaxAttempts int
}

// NewSwapClient creates a new swap client. The optional invoice creator is
// required for ReceiveViaLightning (out-swaps).
func NewSwapClient(server SwapServerConn, daemon DaemonConn,
	log btclog.Logger,
	invoiceGen InvoiceCreator) *SwapClient {

	if log == nil {
		log = btclog.Disabled
	}

	return &SwapClient{
		server:           server,
		daemon:           daemon,
		invoiceGen:       invoiceGen,
		log:              log,
		waitPollInterval: 2 * time.Second,
		waitVHTLCTimeout: 60 * time.Second,
		claimRetryDelay:  time.Second,
		claimMaxAttempts: 10,
	}
}
