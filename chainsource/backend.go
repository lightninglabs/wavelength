package chainsource

import (
	"context"
	"errors"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// ErrPackageMempoolAcceptUnsupported is returned by ChainBackend
// implementations whose underlying RPC cannot test a multi-transaction
// package for mempool acceptance. It is distinct from a per-tx
// "rejected" outcome: the backend never evaluated the package at all.
// Callers that treat package preflight as best-effort should downgrade
// this error to a soft-miss; callers that require package validation
// should surface it as a hard failure.
var ErrPackageMempoolAcceptUnsupported = errors.New("package " +
	"testmempoolaccept not supported by backend")

// MempoolAcceptResult is the per-transaction outcome of a
// TestMempoolAccept call. One result is returned for each input tx, in
// the same order.
type MempoolAcceptResult struct {
	// Txid is the transaction hash the result applies to.
	Txid chainhash.Hash

	// Accepted reports whether the backend would accept the
	// transaction into its mempool.
	Accepted bool

	// Reason carries the backend's human-readable rejection reason
	// when Accepted is false. Empty on acceptance.
	Reason string
}

// ChainBackend defines the interface that must be implemented by all
// blockchain backend providers. This abstraction allows the ChainSource actor
// to work with different backends (lnd's chainntnfs, block explorers like
// mempool.space, or custom implementations) without coupling to any specific
// implementation.
//
// All methods must be safe for concurrent use. Implementations should handle
// their own internal synchronization and connection management.
type ChainBackend interface {
	// EstimateFee returns the estimated fee rate in satoshis per vbyte for
	// a transaction to confirm within the target number of blocks. Returns
	// an error if fee estimation fails or is unavailable.
	EstimateFee(ctx context.Context,
		targetConf uint32) (btcutil.Amount, error)

	// BestBlock returns the current best known block height and hash from
	// the blockchain. This represents the tip of the longest valid chain
	// according to the backend's view.
	BestBlock(ctx context.Context) (int32, chainhash.Hash, error)

	// TestMempoolAccept tests whether one or more transactions would be
	// accepted by the mempool without actually broadcasting them. When
	// len(txs) > 1 the backend must evaluate the transactions as a
	// package (matching Bitcoin Core's testmempoolaccept JSON array
	// form); backends that can only validate individual transactions
	// must return ErrPackageMempoolAcceptUnsupported rather than
	// silently evaluating the first tx in isolation.
	//
	// The returned slice has one entry per input tx, in the same
	// order. Not all backends may support this operation at all; those
	// should return a non-nil error from the single-tx call so callers
	// can distinguish "rejected" from "not evaluated".
	TestMempoolAccept(ctx context.Context,
		txs ...*wire.MsgTx) ([]MempoolAcceptResult, error)

	// BroadcastTx broadcasts a transaction to the network. The label
	// parameter is optional and may be used for wallet tracking. Returns
	// an error if the broadcast fails.
	BroadcastTx(ctx context.Context, tx *wire.MsgTx, label string) error

	// RegisterConf registers for confirmation notifications of a
	// transaction. The registration returns a ConfRegistration that
	// provides channels for receiving confirmation events.
	//
	// Parameters:
	//   - ctx: Context for cancellation
	//   - txid: The transaction ID to monitor (can be nil to match by
	//     script)
	//   - pkScript: The public key script to watch for
	//   - numConfs: Target number of confirmations
	//   - heightHint: Earliest block containing the tx (0 if unknown)
	//   - includeBlock: If true, include the full block in the confirmation
	//     event for merkle proof construction
	//
	// The returned ConfRegistration must have buffered channels and a
	// Cancel function for cleanup.
	RegisterConf(ctx context.Context, txid *chainhash.Hash, pkScript []byte,
		numConfs uint32, heightHint uint32,
		includeBlock bool) (*ConfRegistration, error)

	// RegisterSpend registers for spend notifications of a transaction
	// output. The registration returns a SpendRegistration that provides
	// channels for receiving spend events.
	//
	// Parameters:
	//   - ctx: Context for cancellation
	//   - outpoint: The output to monitor (can be nil to match by script)
	//   - pkScript: The public key script to watch for
	//   - heightHint: Earliest block containing a spend (0 if unknown)
	//
	// The returned SpendRegistration must have buffered channels and a
	// Cancel function for cleanup.
	RegisterSpend(ctx context.Context, outpoint *wire.OutPoint,
		pkScript []byte, heightHint uint32) (*SpendRegistration, error)

	// RegisterBlocks registers for new block notifications. The
	// registration returns a BlockRegistration that provides a channel for
	// receiving block events.
	//
	// The returned BlockRegistration must have a buffered channel and a
	// Cancel function for cleanup. The backend may optionally backfill
	// missed blocks if the client provides a best known block.
	RegisterBlocks(ctx context.Context) (*BlockRegistration, error)

	// SubmitPackage atomically submits a parent+child transaction
	// package to the network. This is required for V3 package relay
	// where the child pays fees for otherwise non-relayable parents.
	SubmitPackage(ctx context.Context, parents []*wire.MsgTx,
		child *wire.MsgTx) error

	// Start initializes the backend and any background processes. This
	// must be called before using any other methods.
	Start() error

	// Stop shuts down the backend and cleans up all resources. All pending
	// registrations will be cancelled.
	Stop() error
}

// ConfRegistration encapsulates the channels and control functions for a
// confirmation registration. This mirrors lnd's chainntnfs.ConfirmationEvent
// structure but provides a backend-agnostic interface.
type ConfRegistration struct {
	// Confirmed is a channel that fires once when the transaction reaches
	// the target number of confirmations. The channel is buffered and will
	// only send a single event.
	Confirmed <-chan *TxConfirmation

	// Cancel is a function that can be called to cancel this registration
	// and clean up resources. After calling Cancel, no more events will be
	// sent on any channels.
	Cancel func()
}

// TxConfirmation contains details about a confirmed transaction. This is sent
// when a monitored transaction reaches its target confirmation count.
type TxConfirmation struct {
	// BlockHash is the hash of the block containing the transaction.
	BlockHash *chainhash.Hash

	// BlockHeight is the height of the block containing the transaction.
	BlockHeight uint32

	// TxIndex is the position of the transaction within the block.
	TxIndex uint32

	// Tx is the confirmed transaction itself.
	Tx *wire.MsgTx

	// Block is the full block containing the transaction. Only populated
	// when the confirmation was registered with IncludeBlock=true. This
	// matches lnd's chainntnfs behavior.
	Block *wire.MsgBlock
}

// SpendRegistration encapsulates the channels and control functions for a
// spend registration. This mirrors lnd's chainntnfs.SpendEvent structure.
type SpendRegistration struct {
	// Spend is a channel that fires when the monitored outpoint is spent.
	// The spending transaction must have at least one confirmation. The
	// channel is buffered and will send an event for each spend (though
	// typically only one unless reorgs occur).
	Spend <-chan *SpendDetail

	// Cancel is a function that can be called to cancel this registration
	// and clean up resources.
	Cancel func()
}

// SpendDetail contains details about a spend of a monitored outpoint. This is
// sent when the outpoint is consumed by a confirmed transaction.
type SpendDetail struct {
	// SpentOutPoint is the outpoint that was spent.
	SpentOutPoint *wire.OutPoint

	// SpenderTxHash is the hash of the spending transaction.
	SpenderTxHash *chainhash.Hash

	// SpendingTx is the full spending transaction.
	SpendingTx *wire.MsgTx

	// SpenderInputIndex is the input index in the spending transaction
	// that consumed the outpoint.
	SpenderInputIndex uint32

	// SpendingHeight is the block height where the spending transaction
	// was confirmed.
	SpendingHeight int32
}

// BlockRegistration encapsulates the channels and control functions for a
// block subscription.
type BlockRegistration struct {
	// Epochs is a channel that sends an event for each new block connected
	// to the chain. The channel is buffered to handle bursts of blocks.
	Epochs <-chan *BlockEpoch

	// Cancel is a function that can be called to cancel this registration
	// and clean up resources.
	Cancel func()
}
