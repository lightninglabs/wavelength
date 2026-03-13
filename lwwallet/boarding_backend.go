package lwwallet

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
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

// BoardingBackendAdapter implements wallet.BoardingBackend by wrapping
// btcwallet.BtcWallet for key derivation and script import, while
// using the Esplora API directly for UTXO queries. This is necessary
// because btcwallet's internal UTXO tracking skips addresses imported
// under non-default key scopes (like LND's m/1017' scope), so we
// query Esplora for UTXOs at imported boarding addresses instead.
type BoardingBackendAdapter struct {
	btcWallet   *btcwallet.BtcWallet
	esplora     *EsploraClient
	chainParams *chaincfg.Params

	// log is the structured logger for this boarding backend
	// instance.
	log btclog.Logger

	// chainKeyScope is the key scope used for script imports.
	// This matches LND's m/1017'/coinType' derivation.
	chainKeyScope waddrmgr.KeyScope

	// mu protects importedAddrs.
	mu sync.Mutex

	// importedAddrs tracks addresses imported via ImportTaprootScript.
	// ListUnspent queries Esplora for UTXOs at each of these
	// addresses, since btcwallet's internal UTXO tracking skips
	// non-default scope addresses in its addRelevantTx credit
	// marking.
	importedAddrs map[string]btcutil.Address
}

// NewBoardingBackendAdapter creates a new boarding backend adapter
// wrapping the given btcwallet instance.
func NewBoardingBackendAdapter(btcw *btcwallet.BtcWallet,
	esplora *EsploraClient, chainParams *chaincfg.Params,
	coinType uint32, logger btclog.Logger) *BoardingBackendAdapter {

	return &BoardingBackendAdapter{
		btcWallet:   btcw,
		esplora:     esplora,
		chainParams: chainParams,
		log:         logger,
		chainKeyScope: waddrmgr.KeyScope{
			Purpose: keychain.BIP0043Purpose,
			Coin:    coinType,
		},
		importedAddrs: make(map[string]btcutil.Address),
	}
}

// DeriveNextKey derives the next key in the specified key family. This
// delegates to btcwallet's keyring which uses the waddrmgr for HD key
// derivation following the m/1017'/coinType'/family'/0/index path.
func (b *BoardingBackendAdapter) DeriveNextKey(ctx context.Context,
	family keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	keyRing := keychain.NewBtcWalletKeyRing(
		b.btcWallet.InternalWallet(),
		b.chainKeyScope.Coin,
	)

	desc, err := keyRing.DeriveNextKey(family)
	if err != nil {
		return nil, fmt.Errorf("derive next key: %w", err)
	}

	b.log.DebugS(ctx, "Derived next key",
		slog.Int("family", int(family)),
		slog.Int("index", int(desc.Index)))

	return &desc, nil
}

// ImportTaprootScript imports a taproot script into btcwallet and
// tracks the resulting address for Esplora-based UTXO queries. The
// import into btcwallet registers the address for chain notifications
// via NotifyReceived, while the local address tracking enables
// ListUnspent to query Esplora directly.
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

	// Track the imported address so ListUnspent can query
	// Esplora for UTXOs at this address. We use Esplora
	// directly because btcwallet's addRelevantTx skips credit
	// marking for non-default scope addresses (1017).
	b.mu.Lock()
	b.importedAddrs[addr.String()] = addr
	b.mu.Unlock()

	b.log.DebugS(ctx,
		"Imported taproot script via btcwallet",
		slog.String("address", addr.String()),
		slog.Int("tracked_addrs", len(b.importedAddrs)))

	return addr, nil
}

// ListUnspent returns UTXOs at all imported boarding addresses by
// querying the Esplora API directly. This bypasses btcwallet's
// internal UTXO tracking, which skips credit marking for addresses
// in non-default key scopes (like LND's m/1017' scope). For each
// imported address, Esplora's /address/:addr/utxo endpoint is
// queried and results are filtered by confirmation count.
func (b *BoardingBackendAdapter) ListUnspent(ctx context.Context,
	minConfs, maxConfs int32) ([]*wallet.Utxo, error) {

	// Treat maxConfs of 0 as "no upper bound" so callers that
	// omit the parameter don't accidentally filter everything.
	if maxConfs == 0 {
		maxConfs = math.MaxInt32
	}

	// Get the current tip height for confirmation calculation.
	tipHeight, err := b.esplora.GetTipHeight()
	if err != nil {
		return nil, fmt.Errorf("get tip height: %w", err)
	}

	b.mu.Lock()
	addrs := make(map[string]btcutil.Address, len(b.importedAddrs))
	for k, v := range b.importedAddrs {
		addrs[k] = v
	}
	b.mu.Unlock()

	var utxos []*wallet.Utxo

	for addrStr, addr := range addrs {
		esploraUtxos, err := b.esplora.GetAddressUtxos(addrStr)
		if err != nil {
			b.log.WarnS(ctx,
				"Failed to query Esplora for address UTXOs",
				err,
				slog.String("address", addrStr))

			continue
		}

		// Compute the pkScript for this address once, then
		// reuse it for all UTXOs at this address.
		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			continue
		}

		for _, eu := range esploraUtxos {
			// Compute confirmations from block height.
			var confs int32
			if eu.Status.Confirmed {
				confs = tipHeight -
					int32(eu.Status.BlockHeight) + 1
			}

			// Apply confirmation filters.
			if confs < minConfs || confs > maxConfs {
				continue
			}

			txid, err := chainhash.NewHashFromStr(eu.Txid)
			if err != nil {
				continue
			}

			utxos = append(utxos, &wallet.Utxo{
				Outpoint: wire.OutPoint{
					Hash:  *txid,
					Index: eu.Vout,
				},
				PkScript:      pkScript,
				Amount:        btcutil.Amount(eu.Value),
				Confirmations: confs,
			})
		}
	}

	b.log.DebugS(ctx, "ListUnspent called",
		slog.Int("min_confs", int(minConfs)),
		slog.Int("max_confs", int(maxConfs)),
		slog.Int("tracked_addrs", len(addrs)),
		slog.Int("utxo_count", len(utxos)))

	return utxos, nil
}

// GetTransaction returns the full transaction and its confirmation block hash
// for the given txid. It first attempts to fetch the raw tx from btcwallet's
// transaction store, falling back to Esplora. The block hash is always fetched
// from Esplora's tx status endpoint so it is available for TxProof
// construction even when catching up on UTXOs confirmed many blocks ago.
func (b *BoardingBackendAdapter) GetTransaction(ctx context.Context,
	txid chainhash.Hash) (*wire.MsgTx, *chainhash.Hash, error) {

	// Try btcwallet's transaction store first for the raw tx.
	tx, err := b.btcWallet.FetchTx(txid)
	if err != nil || tx == nil {
		// Fall back to Esplora for transactions not in the wallet
		// DB.
		b.log.DebugS(ctx,
			"Transaction not in wallet, falling back "+
				"to Esplora",
			slog.String("txid", txid.String()),
		)

		tx, err = b.esplora.GetRawTx(txid)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"get transaction: %w", err,
			)
		}
	}

	// Fetch the confirmation status from Esplora to get the block
	// hash. This is needed for TxProof construction.
	var blockHash *chainhash.Hash

	status, err := b.esplora.GetTxStatus(txid)
	if err != nil {
		b.log.WarnS(ctx,
			"Failed fetching tx status from Esplora", err,
			slog.String("txid", txid.String()),
		)
	} else if status.Confirmed && status.BlockHash != "" {
		h, err := chainhash.NewHashFromStr(status.BlockHash)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"parse block hash: %w", err,
			)
		}

		blockHash = h
	}

	return tx, blockHash, nil
}

// GetBlock returns the full block for the given block hash via the Esplora
// API. This is used to compute merkle inclusion proofs for boarding TxProofs.
func (b *BoardingBackendAdapter) GetBlock(ctx context.Context,
	blockHash chainhash.Hash) (*wire.MsgBlock, error) {

	b.log.DebugS(ctx, "Fetching block from Esplora",
		slog.String("block_hash", blockHash.String()))

	block, err := b.esplora.GetRawBlock(blockHash)
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
