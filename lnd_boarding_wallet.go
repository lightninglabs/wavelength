package main

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
)

// LndBoardingBackend implements the wallet.BoardingBackend interface by
// wrapping an lndclient.WalletKitClient. This provides the concrete
// integration with an LND node for boarding address management.
type LndBoardingBackend struct {
	// walletKit is the LND wallet kit client used for key derivation,
	// script import, and UTXO enumeration.
	walletKit lndclient.WalletKitClient
}

// NewLndBoardingBackend creates a new LND boarding backend.
func NewLndBoardingBackend(
	walletKit lndclient.WalletKitClient,
) *LndBoardingBackend {

	return &LndBoardingBackend{
		walletKit: walletKit,
	}
}

// DeriveNextKey derives the next key in the specified key family using LND's
// key derivation infrastructure.
func (l *LndBoardingBackend) DeriveNextKey(ctx context.Context,
	family keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	keyDesc, err := l.walletKit.DeriveNextKey(ctx, int32(family))
	if err != nil {
		return nil, fmt.Errorf("derive next key: %w", err)
	}

	return keyDesc, nil
}

// ImportTaprootScript imports a taproot script into the LND wallet. After
// import, LND will track UTXOs paying to this script.
func (l *LndBoardingBackend) ImportTaprootScript(ctx context.Context,
	script *waddrmgr.Tapscript) (btcutil.Address, error) {

	addr, err := l.walletKit.ImportTaprootScript(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("import taproot script: %w", err)
	}

	return addr, nil
}

// ListUnspent returns all UTXOs known to the LND wallet with confirmation
// counts between minConfs and maxConfs. Converts from lnwallet.Utxo to the
// wallet package's Utxo type.
func (l *LndBoardingBackend) ListUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*wallet.Utxo, error) {

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

	return utxos, nil
}

// Compile-time check that LndBoardingBackend implements BoardingBackend.
var _ wallet.BoardingBackend = (*LndBoardingBackend)(nil)
