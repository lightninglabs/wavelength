//go:build !js || !wasm

package btcwbackend

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/darepo-client/walletcore"
	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

// BoardingBackendAdapter implements wallet.BoardingBackend by
// embedding walletcore.BoardingBackendBase for shared key derivation
// and script import, with neutrino providing block and transaction
// data. Unlike the lwwallet adapter which bypasses btcwallet for
// UTXO queries, this adapter uses btcwallet's native ListUnspent
// since neutrino's chain sync naturally detects matching outputs via
// compact block filters after address import.
type BoardingBackendAdapter struct {
	// BoardingBackendBase provides shared DeriveNextKey,
	// ImportTaprootScript, and address tracking.
	walletcore.BoardingBackendBase

	neutrinoCS *neutrino.ChainService
	blockCache *blockcache.BlockCache

	chainParams *chaincfg.Params
}

// NewBoardingBackendAdapter creates a new boarding backend adapter
// wrapping the given btcwallet instance with neutrino for chain data.
func NewBoardingBackendAdapter(btcw *btcwallet.BtcWallet,
	neutrinoCS *neutrino.ChainService, blockCache *blockcache.BlockCache,
	chainParams *chaincfg.Params, coinType uint32,
	logger btclog.Logger) *BoardingBackendAdapter {

	return &BoardingBackendAdapter{
		BoardingBackendBase: walletcore.NewBoardingBackendBase(
			btcw, coinType, logger,
		),
		neutrinoCS:  neutrinoCS,
		blockCache:  blockCache,
		chainParams: chainParams,
	}
}

// ListUnspent returns all UTXOs known to btcwallet with confirmation
// counts between minConfs and maxConfs. Unlike the lwwallet adapter
// which filters by imported boarding addresses, this returns all
// watched UTXOs directly from btcwallet. This avoids UTXO loss on
// restart since btcwallet persists its watch set independently of the
// in-memory importedAddrs map.
func (b *BoardingBackendAdapter) ListUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*wallet.Utxo, error) {

	internalWallet := b.BtcWallet.InternalWallet()
	if !internalWallet.ChainSynced() {
		syncedTo := internalWallet.SyncedTo()
		b.Log.DebugS(ctx, "ListUnspent skipped while btcwallet syncing",
			slog.Int("sync_height", int(syncedTo.Height)),
			slog.String("sync_hash", syncedTo.Hash.String()),
		)

		return nil, fmt.Errorf("btcwallet chain sync in progress")
	}

	// Treat maxConfs of 0 as "no upper bound" so callers that
	// omit the parameter don't accidentally filter everything.
	if maxConfs == 0 {
		maxConfs = math.MaxInt32
	}

	// Query btcwallet for all unspent outputs. btcwallet already
	// scopes results to watched scripts (including those imported
	// via ImportTaprootScript), so no additional address filtering
	// is needed.
	results, err := internalWallet.ListUnspent(minConfs, maxConfs, "")
	if err != nil {
		return nil, fmt.Errorf("list unspent: %w", err)
	}

	var utxos []*wallet.Utxo

	for _, r := range results {
		addr, err := btcaddr.DecodeAddress(
			r.Address, b.chainParams,
		)
		if err != nil {
			b.Log.WarnS(ctx, "Failed to decode UTXO address",
				err,
				slog.String("address", r.Address),
			)

			continue
		}

		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			b.Log.WarnS(
				ctx,
				"Failed to create pkScript for address",
				err,
				slog.String("address", r.Address),
			)

			continue
		}

		txid, err := chainhash.NewHashFromStr(r.TxID)
		if err != nil {
			b.Log.WarnS(ctx, "Failed to parse UTXO txid",
				err,
				slog.String("txid", r.TxID),
			)

			continue
		}

		// Convert BTC amount to satoshis.
		amount, err := btcutil.NewAmount(r.Amount)
		if err != nil {
			b.Log.WarnS(ctx,
				"Failed to parse UTXO amount", err)

			continue
		}

		utxos = append(utxos, &wallet.Utxo{
			Outpoint: wire.OutPoint{
				Hash:  *txid,
				Index: r.Vout,
			},
			PkScript:      pkScript,
			Amount:        amount,
			Confirmations: int32(r.Confirmations),
		})
	}

	b.Log.DebugS(ctx, "ListUnspent called",
		slog.Int("min_confs", int(minConfs)),
		slog.Int("max_confs", int(maxConfs)),
		slog.Int("utxo_count", len(utxos)),
	)

	return utxos, nil
}

// GetTransaction returns the full transaction and its confirmation
// block hash for the given txid. It fetches from btcwallet's
// transaction store, which is populated by neutrino's chain sync.
func (b *BoardingBackendAdapter) GetTransaction(ctx context.Context,
	txid chainhash.Hash) (*wallet.TxInfo, error) {

	// Fetch the raw transaction from btcwallet's store.
	tx, err := b.BtcWallet.FetchTx(txid)
	if err != nil || tx == nil {
		b.Log.DebugS(ctx, "Transaction not in wallet store",
			slog.String("txid", txid.String()),
		)

		return nil, fmt.Errorf("transaction %s not found in wallet",
			txid)
	}

	info := &wallet.TxInfo{Tx: tx}

	// Look up the transaction details from btcwallet to get the
	// confirmation block hash and height.
	txDetails, err := b.BtcWallet.InternalWallet().GetTransaction(
		txid,
	)
	if err != nil {

		// We have the tx but no details — return without
		// confirmation metadata.
		return info, nil //nolint:nilerr
	}

	info.BlockHash = txDetails.BlockHash
	info.BlockHeight = txDetails.Height

	return info, nil
}

// GetBlock returns the full block for the given block hash via
// neutrino's P2P network. The block cache is used to avoid redundant
// fetches.
func (b *BoardingBackendAdapter) GetBlock(ctx context.Context,
	blockHash chainhash.Hash) (*wire.MsgBlock, error) {

	b.Log.DebugS(ctx, "Fetching block via neutrino",
		slog.String("block_hash", blockHash.String()),
	)

	block, err := b.blockCache.GetBlock(
		&blockHash,
		func(hash *chainhash.Hash) (*wire.MsgBlock, error) {
			blk, err := b.neutrinoCS.GetBlock(*hash)
			if err != nil {
				return nil, err
			}

			return blk.MsgBlock(), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("get block: %w", err)
	}

	b.Log.DebugS(ctx, "Fetched block successfully",
		slog.String("block_hash", blockHash.String()),
		slog.Int("num_txs", len(block.Transactions)),
	)

	return block, nil
}

// LeaseOutput forwards the output lock to btcwallet's native
// coin-selection lock table. btcwallet persists leases across
// restarts, so once a caller (e.g. the txconfirm CPFP broadcaster)
// reserves a UTXO via LeaseOutput it stays excluded from coin
// selection even if the daemon restarts mid-bump.
//
// The darepo-local wallet.LockID is re-interpreted as wtxmgr.LockID:
// both are [32]byte, so the translation is a direct type cast rather
// than a mapping table. This keeps the LockID stable across the
// interface boundary so ReleaseOutput can use the same identifier
// without any broker-side state.
func (b *BoardingBackendAdapter) LeaseOutput(_ context.Context,
	id wallet.LockID, op wire.OutPoint, expiry time.Duration) (time.Time,
	error) {

	return b.BtcWallet.LeaseOutput(wtxmgr.LockID(id), op, expiry)
}

// ReleaseOutput forwards the unlock to btcwallet's native
// coin-selection lock table. The supplied LockID must match the one
// used at lease time; mismatches surface as an error from btcwallet
// so misuse fails loudly rather than silently releasing someone
// else's lease.
func (b *BoardingBackendAdapter) ReleaseOutput(_ context.Context,
	id wallet.LockID, op wire.OutPoint) error {

	return b.BtcWallet.ReleaseOutput(wtxmgr.LockID(id), op)
}

// Compile-time check that BoardingBackendAdapter implements
// wallet.BoardingBackend and wallet.OutputLeaser.
var (
	_ wallet.BoardingBackend = (*BoardingBackendAdapter)(nil)
	_ wallet.OutputLeaser    = (*BoardingBackendAdapter)(nil)
)
