package lwwallet

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/walletcore"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

// BoardingBackendAdapter implements wallet.BoardingBackend by
// embedding walletcore.BoardingBackendBase for shared key derivation
// and script import, while using the Esplora API directly for UTXO
// queries, transaction fetching, and block fetching. The Esplora
// bypass for UTXOs is necessary because btcwallet's internal UTXO
// tracking skips addresses imported under non-default key scopes
// (like LND's m/1017' scope).
type BoardingBackendAdapter struct {
	// BoardingBackendBase provides shared DeriveNextKey,
	// ImportTaprootScript, and address tracking.
	walletcore.BoardingBackendBase

	esplora     *EsploraClient
	chainParams *chaincfg.Params
}

// NewBoardingBackendAdapter creates a new boarding backend adapter
// wrapping the given btcwallet instance.
func NewBoardingBackendAdapter(btcw *btcwallet.BtcWallet,
	esplora *EsploraClient, chainParams *chaincfg.Params, coinType uint32,
	logger btclog.Logger) *BoardingBackendAdapter {

	return &BoardingBackendAdapter{
		BoardingBackendBase: walletcore.NewBoardingBackendBase(
			btcw, coinType, logger,
		),
		esplora:     esplora,
		chainParams: chainParams,
	}
}

// ListUnspent returns UTXOs at all imported boarding addresses by
// querying the Esplora API directly. This bypasses btcwallet's
// internal UTXO tracking, which skips credit marking for addresses
// in non-default key scopes (like LND's m/1017' scope). For each
// imported address, Esplora's /address/:addr/utxo endpoint is
// queried and results are filtered by confirmation count.
func (b *BoardingBackendAdapter) ListUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*wallet.Utxo, error) {

	// Treat maxConfs of 0 as "no upper bound" so callers that
	// omit the parameter don't accidentally filter everything.
	if maxConfs == 0 {
		maxConfs = math.MaxInt32
	}

	// Get the current tip height for confirmation calculation.
	tipHeight, err := b.esplora.GetTipHeight(ctx)
	if err != nil {
		return nil, fmt.Errorf("get tip height: %w", err)
	}

	addrs := b.SnapshotAddrs()

	var utxos []*wallet.Utxo

	for addrStr, addr := range addrs {
		esploraUtxos, err := b.esplora.GetAddressUtxos(ctx, addrStr)
		if err != nil {
			b.Log.WarnS(
				ctx,
				"Failed to query Esplora for address UTXOs",
				err,
				slog.String("address", addrStr),
			)

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

	b.Log.TraceS(
		ctx, "ListUnspent called",
		slog.Int("min_confs", int(minConfs)),
		slog.Int("max_confs", int(maxConfs)),
		slog.Int("tracked_addrs", len(addrs)),
		slog.Int("utxo_count", len(utxos)),
	)

	return utxos, nil
}

// GetTransaction returns the full transaction and its confirmation metadata
// for the given txid. It first attempts to fetch the raw tx from btcwallet's
// transaction store, falling back to Esplora. The confirmation block data is
// fetched from Esplora's tx status endpoint so it is available for TxProof
// construction even when catching up on UTXOs confirmed many blocks ago.
func (b *BoardingBackendAdapter) GetTransaction(ctx context.Context,
	txid chainhash.Hash) (*wallet.TxInfo, error) {

	// Try btcwallet's transaction store first for the raw tx.
	tx, err := b.BtcWallet.FetchTx(txid)
	if err != nil || tx == nil {
		// Fall back to Esplora for transactions not in the wallet
		// DB.
		b.Log.DebugS(ctx,
			"Transaction not in wallet, falling back "+
				"to Esplora",
			slog.String("txid", txid.String()),
		)

		tx, err = b.esplora.GetRawTx(ctx, txid)
		if err != nil {
			return nil, fmt.Errorf("get transaction: %w", err)
		}
	}

	info := &wallet.TxInfo{Tx: tx}

	// Fetch the confirmation status from Esplora to get the block
	// hash and height. Both are needed for TxProof construction.
	status, err := b.esplora.GetTxStatus(ctx, txid)
	if err != nil {
		b.Log.WarnS(ctx, "Failed fetching tx status from Esplora",
			err,
			slog.String("txid", txid.String()),
		)
	} else if status.Confirmed && status.BlockHash != "" {
		h, err := chainhash.NewHashFromStr(status.BlockHash)
		if err != nil {
			return nil, fmt.Errorf("parse block hash: %w", err)
		}

		info.BlockHash = h
		info.BlockHeight = int32(status.BlockHeight)
	}

	return info, nil
}

// GetBlock returns the full block for the given block hash via the Esplora
// API. This is used to compute merkle inclusion proofs for boarding TxProofs.
func (b *BoardingBackendAdapter) GetBlock(ctx context.Context,
	blockHash chainhash.Hash) (*wire.MsgBlock, error) {

	b.Log.DebugS(ctx, "Fetching block from Esplora",
		slog.String("block_hash", blockHash.String()),
	)

	block, err := b.esplora.GetRawBlock(ctx, blockHash)
	if err != nil {
		return nil, fmt.Errorf("get block: %w", err)
	}

	b.Log.DebugS(ctx, "Fetched block successfully",
		slog.String("block_hash", blockHash.String()),
		slog.Int("num_txs", len(block.Transactions)),
	)

	return block, nil
}

// LeaseOutput forwards the reservation to btcwallet's native
// coin-selection lock table. Even though lwwallet relies on Esplora
// for UTXO enumeration, the underlying btcwallet still owns key
// material and signs transactions, so coin-selection locks held there
// correctly exclude leased outputs from any subsequent signing or
// broadcast path.
//
// The wavelength-local wallet.LockID is re-interpreted as wtxmgr.LockID:
// both are [32]byte, so the translation is a direct cast.
func (b *BoardingBackendAdapter) LeaseOutput(_ context.Context,
	id wallet.LockID, op wire.OutPoint, expiry time.Duration) (time.Time,
	error) {

	return b.BtcWallet.LeaseOutput(wtxmgr.LockID(id), op, expiry)
}

// ReleaseOutput forwards the unlock to btcwallet. The LockID must
// match the one used at lease time.
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
