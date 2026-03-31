package btcwbackend

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

// BoardingBackendAdapter implements wallet.BoardingBackend by wrapping
// btcwallet.BtcWallet for key derivation, script import, and UTXO
// tracking, with neutrino providing block and transaction data. Unlike
// the lwwallet adapter which bypasses btcwallet for UTXO queries,
// this adapter uses btcwallet's native ListUnspent since neutrino's
// chain sync naturally detects matching outputs via compact block
// filters after address import.
type BoardingBackendAdapter struct {
	btcWallet  *btcwallet.BtcWallet
	neutrinoCS *neutrino.ChainService
	blockCache *blockcache.BlockCache

	chainParams *chaincfg.Params

	// log is the structured logger for this boarding backend
	// instance.
	log btclog.Logger

	// keyRing is the cached key ring for HD key derivation.
	keyRing keychain.KeyRing

	// chainKeyScope is the key scope used for script imports.
	// This matches LND's m/1017'/coinType' derivation.
	chainKeyScope waddrmgr.KeyScope

	// mu protects importedAddrs.
	mu sync.Mutex

	// importedAddrs tracks addresses imported via
	// ImportTaprootScript. ListUnspent filters btcwallet's
	// results by these addresses to return only boarding UTXOs.
	//
	// This map is in-memory and is repopulated on daemon restart
	// by the wallet actor, which re-imports all persisted boarding
	// addresses from the database during startup (see
	// wallet.Ark.handleStartupRecovery).
	importedAddrs map[string]btcutil.Address
}

// NewBoardingBackendAdapter creates a new boarding backend adapter
// wrapping the given btcwallet instance with neutrino for chain data.
func NewBoardingBackendAdapter(btcw *btcwallet.BtcWallet,
	neutrinoCS *neutrino.ChainService,
	blockCache *blockcache.BlockCache,
	chainParams *chaincfg.Params, coinType uint32,
	logger btclog.Logger) *BoardingBackendAdapter {

	keyRing := keychain.NewBtcWalletKeyRing(
		btcw.InternalWallet(), coinType,
	)

	return &BoardingBackendAdapter{
		btcWallet:   btcw,
		neutrinoCS:  neutrinoCS,
		blockCache:  blockCache,
		chainParams: chainParams,
		log:         logger,
		keyRing:     keyRing,
		chainKeyScope: waddrmgr.KeyScope{
			Purpose: keychain.BIP0043Purpose,
			Coin:    coinType,
		},
		importedAddrs: make(map[string]btcutil.Address),
	}
}

// DeriveNextKey derives the next key in the specified key family.
// This delegates to btcwallet's keyring which uses the waddrmgr for
// HD key derivation following the m/1017'/coinType'/family'/0/index
// path.
func (b *BoardingBackendAdapter) DeriveNextKey(ctx context.Context,
	family keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	desc, err := b.keyRing.DeriveNextKey(family)
	if err != nil {
		return nil, fmt.Errorf("derive next key: %w", err)
	}

	b.log.DebugS(ctx, "Derived next key",
		slog.Int("family", int(family)),
		slog.Int("index", int(desc.Index)))

	return &desc, nil
}

// ImportTaprootScript imports a taproot script into btcwallet and
// tracks the resulting address for UTXO filtering. After import,
// btcwallet will track UTXOs paying to this address via neutrino's
// compact block filter matching.
func (b *BoardingBackendAdapter) ImportTaprootScript(
	ctx context.Context,
	script *waddrmgr.Tapscript) (btcutil.Address, error) {

	managedAddr, err := b.btcWallet.ImportTaprootScript(
		b.chainKeyScope, script,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"import taproot script: %w", err,
		)
	}

	addr := managedAddr.Address()

	// Track the imported address so ListUnspent can filter
	// btcwallet's results to only return boarding UTXOs.
	b.mu.Lock()
	b.importedAddrs[addr.String()] = addr
	numAddrs := len(b.importedAddrs)
	b.mu.Unlock()

	b.log.DebugS(ctx,
		"Imported taproot script via btcwallet",
		slog.String("address", addr.String()),
		slog.Int("tracked_addrs", numAddrs))

	return addr, nil
}

// ListUnspent returns UTXOs at all imported boarding addresses by
// querying btcwallet's UTXO set. After ImportTaprootScript,
// btcwallet's neutrino-backed sync detects matching outputs via
// compact block filters. We filter the results to only include
// UTXOs at tracked boarding addresses.
func (b *BoardingBackendAdapter) ListUnspent(ctx context.Context,
	minConfs, maxConfs int32) ([]*wallet.Utxo, error) {

	// Treat maxConfs of 0 as "no upper bound" so callers that
	// omit the parameter don't accidentally filter everything.
	if maxConfs == 0 {
		maxConfs = math.MaxInt32
	}

	b.mu.Lock()
	addrs := make(map[string]btcutil.Address, len(b.importedAddrs))
	for k, v := range b.importedAddrs {
		addrs[k] = v
	}
	b.mu.Unlock()

	if len(addrs) == 0 {
		return nil, nil
	}

	// Query btcwallet for all unspent outputs. We use the
	// internal wallet's ListUnspent which returns results for
	// all accounts including imported scripts.
	results, err := b.btcWallet.InternalWallet().ListUnspent(
		minConfs, maxConfs, "",
	)
	if err != nil {
		return nil, fmt.Errorf("list unspent: %w", err)
	}

	var utxos []*wallet.Utxo

	for _, r := range results {
		// Only include UTXOs at imported boarding addresses.
		if _, ok := addrs[r.Address]; !ok {
			continue
		}

		addr := addrs[r.Address]

		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			b.log.WarnS(ctx,
				"Failed to create pkScript for address",
				err,
				slog.String("address", r.Address))

			continue
		}

		txid, err := chainhash.NewHashFromStr(r.TxID)
		if err != nil {
			b.log.WarnS(ctx,
				"Failed to parse UTXO txid", err,
				slog.String("txid", r.TxID))

			continue
		}

		// Convert BTC amount to satoshis.
		amount, err := btcutil.NewAmount(r.Amount)
		if err != nil {
			b.log.WarnS(ctx,
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

	b.log.DebugS(ctx, "ListUnspent called",
		slog.Int("min_confs", int(minConfs)),
		slog.Int("max_confs", int(maxConfs)),
		slog.Int("tracked_addrs", len(addrs)),
		slog.Int("utxo_count", len(utxos)))

	return utxos, nil
}

// GetTransaction returns the full transaction and its confirmation
// block hash for the given txid. It fetches from btcwallet's
// transaction store, which is populated by neutrino's chain sync.
func (b *BoardingBackendAdapter) GetTransaction(ctx context.Context,
	txid chainhash.Hash) (*wire.MsgTx, *chainhash.Hash, error) {

	// Try btcwallet's transaction store first.
	tx, err := b.btcWallet.FetchTx(txid)
	if err != nil || tx == nil {
		b.log.DebugS(ctx,
			"Transaction not in wallet store",
			slog.String("txid", txid.String()),
		)

		return nil, nil, fmt.Errorf(
			"transaction %s not found in wallet", txid,
		)
	}

	// Look up the transaction details from btcwallet to get the
	// block hash.
	txDetails, err := b.btcWallet.InternalWallet().GetTransaction(
		txid,
	)
	if err != nil {
		// We have the tx but no details — return without block
		// hash.
		return tx, nil, nil
	}

	return tx, txDetails.BlockHash, nil
}

// GetBlock returns the full block for the given block hash via
// neutrino's P2P network. The block cache is used to avoid redundant
// fetches.
func (b *BoardingBackendAdapter) GetBlock(ctx context.Context,
	blockHash chainhash.Hash) (*wire.MsgBlock, error) {

	b.log.DebugS(ctx, "Fetching block via neutrino",
		slog.String("block_hash", blockHash.String()))

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

	b.log.DebugS(ctx, "Fetched block successfully",
		slog.String("block_hash", blockHash.String()),
		slog.Int("num_txs", len(block.Transactions)))

	return block, nil
}

// Compile-time check that BoardingBackendAdapter implements
// wallet.BoardingBackend.
var _ wallet.BoardingBackend = (*BoardingBackendAdapter)(nil)
