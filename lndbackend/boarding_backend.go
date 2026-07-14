// Package lndbackend provides lndclient-backed implementations of wallet
// interfaces for connecting to remote LND nodes.
package lndbackend

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// BoardingBackend implements the wallet.BoardingBackend interface by
// wrapping an lndclient.WalletKitClient and ChainKitClient. This
// provides the concrete integration with an LND node for boarding
// address management and block retrieval.
type BoardingBackend struct {
	// walletKit is the LND wallet kit client used for key derivation,
	// script import, and UTXO enumeration.
	walletKit lndclient.WalletKitClient

	// chainKit is the LND chain kit client used for block retrieval.
	// This enables TxProof construction when boarding UTXOs confirm.
	chainKit lndclient.ChainKitClient

	// Log is an optional logger for this backend. If None, the backend
	// falls back to extracting a logger from context.
	Log fn.Option[btclog.Logger]
}

// NewBoardingBackend creates a new LND boarding backend.
func NewBoardingBackend(walletKit lndclient.WalletKitClient,
	chainKit lndclient.ChainKitClient) *BoardingBackend {

	return &BoardingBackend{
		walletKit: walletKit,
		chainKit:  chainKit,
	}
}

// WalletKit returns the underlying LND WalletKit client for
// operations not covered by the BoardingBackend interface.
func (l *BoardingBackend) WalletKit() lndclient.WalletKitClient {
	return l.walletKit
}

// logger returns the configured logger, falling back to the context logger.
func (l *BoardingBackend) logger(ctx context.Context) btclog.Logger {
	return l.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// DeriveNextKey derives the next key in the specified key family using LND's
// key derivation infrastructure.
func (l *BoardingBackend) DeriveNextKey(ctx context.Context,
	family keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	l.logger(ctx).DebugS(ctx, "Deriving next key from LND wallet",
		slog.Int("key_family", int(family)),
	)

	keyDesc, err := l.walletKit.DeriveNextKey(ctx, int32(family))
	if err != nil {
		return nil, fmt.Errorf("derive next key: %w", err)
	}

	l.logger(ctx).DebugS(ctx, "Derived next key successfully",
		slog.Int("key_family", int(family)),
		slog.Int("key_index", int(keyDesc.Index)),
	)

	return keyDesc, nil
}

// ImportTaprootScript imports a taproot script into the LND wallet. After
// import, LND will track UTXOs paying to this script.
func (l *BoardingBackend) ImportTaprootScript(ctx context.Context,
	script *waddrmgr.Tapscript) (btcaddr.Address, error) {

	l.logger(ctx).DebugS(ctx, "Importing taproot script into LND wallet")

	addr, err := l.walletKit.ImportTaprootScript(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("import taproot script: %w", err)
	}

	l.logger(ctx).InfoS(ctx, "Imported taproot script successfully",
		slog.String("address", addr.String()),
	)

	return addr, nil
}

// ListUnspent returns all UTXOs known to the LND wallet with confirmation
// counts between minConfs and maxConfs. Converts from lnwallet.Utxo to the
// wallet package's Utxo type.
//
// The result spans every wallet account, including imported watch-only
// script outputs (e.g. boarding scripts tracked via ImportTaprootScript).
// Callers that need outputs the wallet can unilaterally sign, such as CPFP
// fee-input selection, must use ListUnspentDefaultAccount instead.
func (l *BoardingBackend) ListUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*wallet.Utxo, error) {

	return l.listUnspent(ctx, minConfs, maxConfs)
}

// ListUnspentDefaultAccount returns UTXOs from LND's default wallet account
// only. This excludes imported watch-only script outputs (boarding and exit
// scripts imported via ImportTaprootScript), which the wallet merely tracks
// and cannot unilaterally sign. CPFP fee-input selection must use this
// variant: offering a watch-only output as a fee input makes the child PSBT
// unsignable and the fee bump fails with "PSBT is not finalizable".
func (l *BoardingBackend) ListUnspentDefaultAccount(ctx context.Context,
	minConfs, maxConfs int32) ([]*wallet.Utxo, error) {

	return l.listUnspent(
		ctx, minConfs, maxConfs,
		lndclient.WithUnspentAccount(lnwallet.DefaultAccountName),
	)
}

// listUnspent performs the walletKit ListUnspent call with the supplied
// options and converts the results to the wallet package's Utxo type.
func (l *BoardingBackend) listUnspent(ctx context.Context, minConfs,
	maxConfs int32, opts ...lndclient.ListUnspentOption) ([]*wallet.Utxo,
	error) {

	l.logger(ctx).TraceS(ctx, "Listing unspent UTXOs from LND wallet",
		slog.Int("min_confs", int(minConfs)),
		slog.Int("max_confs", int(maxConfs)),
	)

	lndUtxos, err := l.walletKit.ListUnspent(
		ctx, minConfs, maxConfs, opts...,
	)
	if err != nil {
		return nil, fmt.Errorf("list unspent: %w", err)
	}

	utxos := make([]*wallet.Utxo, 0, len(lndUtxos))
	for _, lndUtxo := range lndUtxos {
		utxo := &wallet.Utxo{
			Outpoint: wire.OutPoint{
				Hash:  lndUtxo.OutPoint.Hash,
				Index: lndUtxo.OutPoint.Index,
			},
			PkScript:      lndUtxo.PkScript,
			Amount:        lndUtxo.Value,
			Confirmations: int32(lndUtxo.Confirmations),
		}

		utxos = append(utxos, utxo)
	}

	l.logger(ctx).TraceS(ctx, "Listed unspent UTXOs from LND wallet",
		slog.Int("num_utxos", len(utxos)),
	)

	return utxos, nil
}

// GetTransaction returns the full transaction and its confirmation metadata
// for a given txid. The block data is extracted from LND's
// TransactionDetail so callers can build TxProofs against the correct block
// even when catching up on UTXOs confirmed many blocks ago.
func (l *BoardingBackend) GetTransaction(ctx context.Context,
	txid chainhash.Hash) (*wallet.TxInfo, error) {

	l.logger(ctx).DebugS(ctx, "Fetching transaction from LND",
		btclog.Hex("txid", txid[:]),
	)

	txn, err := l.walletKit.GetTransaction(ctx, txid)
	if err != nil {
		return nil, fmt.Errorf("get transaction: %w", err)
	}

	info := &wallet.TxInfo{
		Tx:          txn.Tx,
		BlockHeight: txn.BlockHeight,
	}

	// Parse the confirmation block hash from the TransactionDetail.
	// This is empty for unconfirmed transactions.
	if txn.BlockHash != "" {
		h, err := chainhash.NewHashFromStr(txn.BlockHash)
		if err != nil {
			return nil, fmt.Errorf("parse block hash: %w", err)
		}

		info.BlockHash = h
	}

	l.logger(ctx).DebugS(ctx, "Fetched transaction successfully",
		btclog.Hex("txid", txid[:]),
		slog.Int("num_inputs", len(txn.Tx.TxIn)),
		slog.Int("num_outputs", len(txn.Tx.TxOut)),
	)

	return info, nil
}

// GetBlock returns the full block for the given block hash via LND's ChainKit
// RPC. This is used to compute merkle inclusion proofs for boarding TxProofs.
func (l *BoardingBackend) GetBlock(ctx context.Context,
	blockHash chainhash.Hash) (*wire.MsgBlock, error) {

	l.logger(ctx).DebugS(ctx, "Fetching block from LND",
		btclog.Hex("block_hash", blockHash[:]),
	)

	block, err := l.chainKit.GetBlock(ctx, blockHash)
	if err != nil {
		return nil, fmt.Errorf("get block: %w", err)
	}

	l.logger(ctx).DebugS(ctx, "Fetched block successfully",
		btclog.Hex("block_hash", blockHash[:]),
		slog.Int("num_txs", len(block.Transactions)),
	)

	return block, nil
}

// LeaseOutput forwards the reservation to LND's WalletKit, which
// translates into an RPC call that sets the coin-selection lock on
// the remote LND node. The wavelength-local wallet.LockID is passed
// through as wtxmgr.LockID — both are [32]byte so the translation is
// a direct cast rather than a mapping table, and the LockID therefore
// round-trips across restarts for release.
func (l *BoardingBackend) LeaseOutput(ctx context.Context, id wallet.LockID,
	op wire.OutPoint, expiry time.Duration) (time.Time, error) {

	return l.walletKit.LeaseOutput(ctx, wtxmgr.LockID(id), op, expiry)
}

// ReleaseOutput forwards the unlock to LND's WalletKit. The supplied
// LockID must match the one used at lease time; mismatches surface as
// an RPC error from LND so misuse fails loudly rather than silently
// releasing someone else's lease.
func (l *BoardingBackend) ReleaseOutput(ctx context.Context, id wallet.LockID,
	op wire.OutPoint) error {

	return l.walletKit.ReleaseOutput(ctx, wtxmgr.LockID(id), op)
}

// Compile-time check that BoardingBackend implements wallet.BoardingBackend
// and wallet.OutputLeaser.
var (
	_ wallet.BoardingBackend = (*BoardingBackend)(nil)
	_ wallet.OutputLeaser    = (*BoardingBackend)(nil)
)
