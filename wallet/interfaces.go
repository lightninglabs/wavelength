package wallet

import (
	"context"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/wavelength/walletcore"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

// LockID is a 32-byte caller-scoped identifier assigned when leasing a
// wallet output, and re-supplied when releasing it. Each subsystem should
// derive its own LockID from a stable, human-readable prefix (for example
// the first 32 bytes of sha256("txconfirm")) so that two subsystems cannot
// accidentally release each other's leases and so the ID is stable across
// restarts.
//
// The canonical declaration lives in walletcore so packages that would
// otherwise import-cycle with wallet (for example txconfirm) can depend on
// a single shared type without going through wallet.
type LockID = walletcore.LockID

// OutputLeaser is implemented by wallet backends that let callers exclude
// specific UTXOs from the wallet's own coin-selection pool for a bounded
// duration. The canonical declaration lives in walletcore so packages that
// would otherwise import-cycle with wallet can depend on a single shared
// interface without going through wallet.
type OutputLeaser = walletcore.OutputLeaser

// VTXODescriptor contains the VTXO information needed by the wallet to build
// intent packages for round registration. This is a wallet-level view that
// avoids importing the vtxo package (which would cause an import cycle).
type VTXODescriptor struct {
	// Outpoint identifies the VTXO's location in the virtual transaction
	// tree.
	Outpoint wire.OutPoint

	// Amount is the value of this VTXO in satoshis.
	Amount btcutil.Amount

	// PolicyTemplate is the semantic arkscript policy for this VTXO.
	PolicyTemplate []byte

	// PkScript is the output script for this VTXO.
	PkScript []byte

	// Expiry is the CSV delay for the unilateral exit path. This
	// corresponds to vtxo.Descriptor.RelativeExpiry.
	Expiry uint32

	// ClientKey is the client's key descriptor for this VTXO.
	ClientKey keychain.KeyDescriptor

	// OperatorKey is the operator's public key for collaborative spends.
	OperatorKey *btcec.PublicKey
}

// VTXOReader provides read-only access to VTXO descriptors. The wallet uses
// this to load VTXO data when building intent packages for round registration.
// Implementors convert from their internal VTXO representation (e.g.,
// vtxo.Descriptor) to the wallet-level VTXODescriptor.
type VTXOReader interface {
	// GetVTXO retrieves a VTXO descriptor by its outpoint. Returns an
	// error if the VTXO is not found.
	GetVTXO(ctx context.Context,
		outpoint wire.OutPoint) (*VTXODescriptor, error)
}

// VTXOReaderFunc is an adapter to allow the use of ordinary functions as
// VTXOReader. If f is a function with the appropriate signature,
// VTXOReaderFunc(f) is a VTXOReader that calls f.
type VTXOReaderFunc func(ctx context.Context,
	outpoint wire.OutPoint) (*VTXODescriptor, error)

// GetVTXO calls f(ctx, outpoint).
func (f VTXOReaderFunc) GetVTXO(ctx context.Context, outpoint wire.OutPoint) (
	*VTXODescriptor, error) {

	return f(ctx, outpoint)
}

// Ask documents a request-response message pair. The Req type is sent via
// actor.Ask, and the Resp type is returned. This is used purely for
// documentation to provide a quick reference of available operations.
type Ask[Req WalletMsg, Resp WalletResp] struct{}

// Tell documents a fire-and-forget message. The Msg type is sent via
// actor.Tell with no response expected. This is used purely for documentation.
type Tell[Msg WalletMsg] struct{}

// MessageSpec documents all message types supported by the boarding wallet
// actor. This provides a quick reference similar to a protobuf service
// definition, showing request/response pairs and events at a glance.
//
// Request-response operations use Ask, which returns a Future:
//
//	future := walletRef.Ask(ctx, &CreateBoardingAddressRequest{...})
//	result := future.Await(ctx)
//
// Fire-and-forget messages use Tell:
//
//	walletRef.Tell(ctx, BlockEpochNotification{...})
var MessageSpec = struct {
	// CreateBoardingAddress derives a new boarding address with the given
	// operator key and exit delay.
	CreateBoardingAddress Ask[
		*CreateBoardingAddressRequest,
		*CreateBoardingAddressResponse,
	]

	// GetActiveBoardingAddresses returns all boarding addresses currently
	// being monitored by the wallet.
	GetActiveBoardingAddresses Ask[
		*GetActiveBoardingAddressesRequest,
		*GetActiveBoardingAddressesResponse,
	]

	// GetBoardingBalance returns the balance breakdown across boarding
	// lifecycle states.
	GetBoardingBalance Ask[
		*GetBoardingBalanceRequest,
		*GetBoardingBalanceResponse,
	]

	// RegisterConfirmationNotifier subscribes an actor to receive
	// BoardingUtxoConfirmedEvent notifications when new UTXOs confirm.
	RegisterConfirmationNotifier Ask[
		*RegisterConfirmationNotifierRequest,
		*RegisterConfirmationNotifierResponse,
	]

	// GetConfirmedBoardingIntents returns the currently confirmed boarding
	// intents tracked by the wallet.
	GetConfirmedBoardingIntents Ask[
		*GetConfirmedBoardingIntentsRequest,
		*GetConfirmedBoardingIntentsResponse,
	]

	// UnregisterConfirmationNotifier removes a previously registered
	// confirmation notifier subscription.
	UnregisterConfirmationNotifier Ask[
		*UnregisterConfirmationNotifierRequest,
		*UnregisterConfirmationNotifierResponse,
	]

	// BlockEpochNotification is received from the chain source when a new
	// block is connected. Triggers UTXO polling and confirmation checks.
	BlockEpochNotification Tell[BlockEpochNotification]

	// BoardingUtxoConfirmedEvent is sent TO registered notifiers (not to
	// this actor) when a boarding UTXO confirms. Included here for
	// completeness of the message surface.
	BoardingUtxoConfirmedEvent Tell[BoardingUtxoConfirmedEvent]
}{}

// BoardingBackend abstracts the operations needed to interact with the
// underlying LND wallet for boarding address management. This interface
// provides key derivation, address import, and UTXO enumeration capabilities.
type BoardingBackend interface {
	// DeriveNextKey derives the next key in the specified key family. This
	// is used to generate new client keys for boarding addresses. The key
	// descriptor includes both the public key and its derivation path,
	// enabling later signing operations.
	DeriveNextKey(ctx context.Context,
		key keychain.KeyFamily) (*keychain.KeyDescriptor, error)

	// ImportTaprootScript imports a constructed taproot script into the
	// LND wallet. After import, LND will track UTXOs paying to this script
	// and include them in ListUnspent queries. The script contains both
	// collaborative (2-of-2) and timeout (CSV) spending paths.
	ImportTaprootScript(ctx context.Context,
		script *waddrmgr.Tapscript) (btcaddr.Address, error)

	// ListUnspent returns all UTXOs known to the wallet with confirmation
	// counts between minConfs and maxConfs (inclusive). This is used to
	// poll for new boarding UTXOs on each block.
	ListUnspent(ctx context.Context, minConfs,
		maxConfs int32) ([]*Utxo, error)

	// GetTransaction returns the full transaction and its
	// confirmation metadata. The confirmation block hash and height
	// are needed for TxProof construction — using the actual
	// confirmation block ensures proofs are correct even for UTXOs
	// discovered during catch-up after downtime.
	GetTransaction(ctx context.Context,
		txid chainhash.Hash) (*TxInfo, error)

	// GetBlock returns the full block for the given block hash. This is
	// used to compute merkle inclusion proofs (TxProof) when a boarding
	// UTXO confirms, enabling SPV verification on the server side.
	GetBlock(ctx context.Context,
		blockHash chainhash.Hash) (*wire.MsgBlock, error)
}

// TxInfo contains a fetched transaction and its on-chain confirmation
// metadata. When the transaction is unconfirmed, BlockHash is nil and
// BlockHeight is 0.
type TxInfo struct {
	// Tx is the full deserialized transaction.
	Tx *wire.MsgTx

	// BlockHash is the hash of the block that confirms the
	// transaction. Nil when unconfirmed.
	BlockHash *chainhash.Hash

	// BlockHeight is the height of the confirmation block. Zero
	// when unconfirmed.
	BlockHeight int32
}

// Utxo represents an unspent transaction output returned by ListUnspent.
// The canonical declaration lives in walletcore so packages that would
// otherwise import-cycle with wallet can depend on a single shared type
// without going through wallet.
type Utxo = walletcore.Utxo

// UtxoKey is used as a key in fn.Set to track which UTXOs we've already
// processed. This enables efficient deduplication when polling ListUnspent
// repeatedly.
type UtxoKey = wire.OutPoint

// NewUtxoKey creates a UtxoKey from a wire.OutPoint.
func NewUtxoKey(op wire.OutPoint) UtxoKey {
	return op
}

// BoardingStatus captures lifecycle of a boarding intent. Intents are only
// created after the boarding UTXO has been confirmed on-chain, so there is no
// "pending" or "waiting" status.
type BoardingStatus uint8

const (
	// BoardingStatusConfirmed indicates that the boarding UTXO has been
	// confirmed and the intent is ready to be included in a round
	// registration.
	BoardingStatusConfirmed BoardingStatus = 0

	// BoardingStatusAdopted indicates that the boarding intent has been
	// included in a round that has been frozen/finalized on disk. The
	// intent can no longer be used in other rounds.
	BoardingStatusAdopted BoardingStatus = 1

	// BoardingStatusFailed indicates that the boarding process failed for
	// this intent. This could be due to validation errors, server
	// rejection, or round failure. Recovery may be possible via CSV
	// timeout path.
	BoardingStatusFailed BoardingStatus = 2

	// BoardingStatusExpired indicates that the boarding UTXO's CSV timeout
	// has elapsed. The client can now spend via the unilateral timeout path
	// to recover funds.
	BoardingStatusExpired BoardingStatus = 3

	// BoardingStatusSwept indicates that the boarding UTXO has been spent
	// via the CSV timeout path to recover funds.
	BoardingStatusSwept BoardingStatus = 4

	// BoardingStatusSweepPending indicates that the boarding UTXO has been
	// included in a published timeout-path sweep transaction and is waiting
	// for the chain backend to report the outpoint as spent.
	BoardingStatusSweepPending BoardingStatus = 5
)

// BoardingAddress represents a derived boarding address with all the
// information needed to monitor and spend from it. This type holds both the
// on-chain address and the cryptographic material (keys, scripts) used to
// construct collaborative and timeout spending paths.
type BoardingAddress struct {
	// Address is the bech32m taproot address that can receive funds.
	Address btcaddr.Address

	// Tapscript contains the full taproot script tree with both
	// collaborative (multisig) and timeout (CSV) spending paths.
	Tapscript *waddrmgr.Tapscript

	// KeyDesc is the client's key descriptor used in both spending paths.
	// Contains both the public key and its derivation path.
	KeyDesc keychain.KeyDescriptor

	// OperatorKey is the operator's public key used in the collaborative
	// spending path.
	OperatorKey *btcec.PublicKey

	// ExitDelay is the CSV delay (in blocks) that must expire before the
	// client can spend unilaterally via the timeout path.
	ExitDelay uint32
}

// BoardingChainInfo tracks the chain related information for a given boarding
// intent.
type BoardingChainInfo struct {
	// ConfHeight is the confirmation height of the boarding output.
	ConfHeight int32

	// ConfHash is the confirmation block hash of the boarding output.
	ConfHash chainhash.Hash

	// ConfTx is the confirmation transaction of the boarding output.
	ConfTx *wire.MsgTx

	// OutPoint is the boarding output outpoint.
	OutPoint wire.OutPoint

	// Amount is the boarding output amount.
	Amount btcutil.Amount

	// TxProof is the SPV proof that the boarding transaction exists in a
	// confirmed block. This includes the merkle proof, block header, and
	// output construction details needed for server verification without
	// querying its own chain source. None if the proof hasn't been
	// constructed yet (e.g., block data not available).
	TxProof fn.Option[proof.TxProof]
}

// BoardingIntent captures one confirmed boarding input. Intents are only
// created once a boarding UTXO has been confirmed on-chain.
type BoardingIntent struct {
	// Address is the boarding address details. This includes which keys
	// were used, the CSV time lock, etc.
	Address BoardingAddress

	// Outpoint is the outpoint of the boarding UTXO.
	Outpoint wire.OutPoint

	// ChainInfo captures the on-chain status of the boarding input.
	// This is always populated since intents are only created after
	// confirmation.
	ChainInfo BoardingChainInfo

	// Status is the current status of the boarding intent.
	Status BoardingStatus
}

// BoardingStore defines the storage interface for boarding addresses and
// intents used by the wallet actor. Embeds PendingIntentStore so the
// restart-safe intent-outbox surface is part of the same contract. One
// logical store covers boarding addresses, intents, and pending-intent
// replay rows — splitting further would fragment transaction boundaries
// across the same SQL table family.
//
//nolint:interfacebloat
type BoardingStore interface {
	PendingIntentStore

	// InsertBoardingAddress persists a boarding address when it is first
	// created. This method is idempotent.
	InsertBoardingAddress(ctx context.Context, addr *BoardingAddress) error

	// LookupBoardingAddress retrieves a boarding address by its pkScript.
	// Returns an error if the address is not found.
	LookupBoardingAddress(ctx context.Context,
		pkScript []byte) (*BoardingAddress, error)

	// ListAllBoardingAddresses returns all persisted boarding addresses.
	ListAllBoardingAddresses(ctx context.Context) (
		[]*BoardingAddress,
		error,
	)

	// InsertBoardingIntents persists one or more boarding intents.
	InsertBoardingIntents(ctx context.Context,
		intents ...BoardingIntent) error

	// FetchBoardingIntents returns all boarding intents that are currently
	// in progress (not yet completed).
	FetchBoardingIntents(ctx context.Context) ([]BoardingIntent, error)

	// FetchBoardingIntentOutpoints returns just the outpoints of all
	// boarding intents. This is more efficient than FetchBoardingIntents
	// when only the outpoints are needed.
	FetchBoardingIntentOutpoints(ctx context.Context) (
		[]wire.OutPoint,
		error,
	)

	// FetchBoardingIntentsByStatus returns all boarding intents matching
	// the given status.
	FetchBoardingIntentsByStatus(ctx context.Context,
		status BoardingStatus) ([]BoardingIntent, error)

	// FetchBoardingIntentsByStatusAndMinHeight returns all boarding intents
	// matching the given status with confirmation height >= minHeight.
	FetchBoardingIntentsByStatusAndMinHeight(ctx context.Context,
		status BoardingStatus,
		minHeight int32) ([]BoardingIntent, error)

	// GetIntent retrieves a boarding intent by its outpoint (primary key).
	GetIntent(ctx context.Context,
		outpoint wire.OutPoint) (*BoardingIntent, error)

	// LookupIntentByScript returns the stored intent associated with a
	// boarding pkScript.
	LookupIntentByScript(ctx context.Context,
		pkScript []byte) (*BoardingIntent, error)
}
