// Package lndbackend provides lndclient-backed implementations of wallet
// interfaces for connecting to remote LND nodes.
package lndbackend

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
)

// BoardingBackend implements the wallet.BoardingBackend interface by
// wrapping an lndclient.WalletKitClient. This provides the concrete
// integration with an LND node for boarding address management.
type BoardingBackend struct {
	// walletKit is the LND wallet kit client used for key derivation,
	// script import, and UTXO enumeration.
	walletKit lndclient.WalletKitClient
}

// NewBoardingBackend creates a new LND boarding backend.
func NewBoardingBackend(
	walletKit lndclient.WalletKitClient) *BoardingBackend {

	return &BoardingBackend{
		walletKit: walletKit,
	}
}

// DeriveNextKey derives the next key in the specified key family using LND's
// key derivation infrastructure.
func (l *BoardingBackend) DeriveNextKey(ctx context.Context,
	family keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	log.DebugS(ctx, "Deriving next key from LND wallet",
		slog.Int("key_family", int(family)))

	keyDesc, err := l.walletKit.DeriveNextKey(ctx, int32(family))
	if err != nil {
		return nil, fmt.Errorf("derive next key: %w", err)
	}

	log.DebugS(ctx, "Derived next key successfully",
		slog.Int("key_family", int(family)),
		slog.Int("key_index", int(keyDesc.Index)))

	return keyDesc, nil
}

// ImportTaprootScript imports a taproot script into the LND wallet. After
// import, LND will track UTXOs paying to this script.
func (l *BoardingBackend) ImportTaprootScript(ctx context.Context,
	script *waddrmgr.Tapscript) (btcutil.Address, error) {

	log.DebugS(ctx, "Importing taproot script into LND wallet")

	addr, err := l.walletKit.ImportTaprootScript(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("import taproot script: %w", err)
	}

	log.InfoS(ctx, "Imported taproot script successfully",
		slog.String("address", addr.String()))

	return addr, nil
}

// ListUnspent returns all UTXOs known to the LND wallet with confirmation
// counts between minConfs and maxConfs. Converts from lnwallet.Utxo to the
// wallet package's Utxo type.
func (l *BoardingBackend) ListUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*wallet.Utxo, error) {

	log.DebugS(ctx, "Listing unspent UTXOs from LND wallet",
		slog.Int("min_confs", int(minConfs)),
		slog.Int("max_confs", int(maxConfs)))

	lndUtxos, err := l.walletKit.ListUnspent(ctx, minConfs, maxConfs)
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

	log.DebugS(ctx, "Listed unspent UTXOs from LND wallet",
		slog.Int("num_utxos", len(utxos)))

	return utxos, nil
}

// GetTransaction returns the full transaction for a given txid. This is used
// to fetch transaction data when a new boarding UTXO is detected, allowing the
// round actor to validate outputs and construct TxProofs.
func (l *BoardingBackend) GetTransaction(ctx context.Context,
	txid chainhash.Hash) (*wire.MsgTx, error) {

	log.DebugS(ctx, "Fetching transaction from LND",
		btclog.Hex("txid", txid[:]))

	txn, err := l.walletKit.GetTransaction(ctx, txid)
	if err != nil {
		return nil, fmt.Errorf("get transaction: %w", err)
	}

	log.DebugS(ctx, "Fetched transaction successfully",
		btclog.Hex("txid", txid[:]),
		slog.Int("num_inputs", len(txn.Tx.TxIn)),
		slog.Int("num_outputs", len(txn.Tx.TxOut)))

	return txn.Tx, nil
}

// Compile-time check that BoardingBackend implements wallet.BoardingBackend.
var _ wallet.BoardingBackend = (*BoardingBackend)(nil)
