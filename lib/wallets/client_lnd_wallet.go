package wallets

import (
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

const (
	// boardingAddrKeyFamily is the key family we use to derive keys for
	// boarding addresses.
	boardingAddrKeyFamily = 43000

	vtxoAddrKeyFamily = 43001

	musigSigningKeyFamily = 43002
)

type clientLndWallet struct {
	chainParams *chaincfg.Params
	keyRing     keychain.SecretKeyRing
	wallet      lnwallet.WalletController
	input.Signer
}

func NewClientLndWallet(chainParams *chaincfg.Params,
	keyRing keychain.SecretKeyRing, wallet lnwallet.WalletController,
	signer input.Signer) ClientWallet {

	return &clientLndWallet{
		chainParams: chainParams,
		keyRing:     keyRing,
		wallet:      wallet,
		Signer:      signer,
	}
}

func (c *clientLndWallet) GetUTXOsForAddress(address btcutil.Address) (
	[]*lnwallet.Utxo, error) {

	utxos, err := c.wallet.ListUnspentWitness(0, math.MaxInt32, "")
	if err != nil {
		return nil, err
	}

	for _, utxo := range utxos {
		if utxo.PkScript == nil {
			continue
		}

		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			utxo.PkScript, c.chainParams,
		)
		if err != nil {
			return nil, err
		}

		if len(addrs) == 0 {
			continue
		}

		if addrs[0].EncodeAddress() == address.EncodeAddress() {
			return []*lnwallet.Utxo{utxo}, nil
		}
	}

	return nil, nil
}

func (c *clientLndWallet) NextBoardingKey() (keychain.KeyDescriptor, error) {
	return c.keyRing.DeriveNextKey(boardingAddrKeyFamily)
}

func (c *clientLndWallet) NextVTXOKey() (keychain.KeyDescriptor, error) {
	return c.keyRing.DeriveNextKey(vtxoAddrKeyFamily)
}

func (c *clientLndWallet) NextMusig2SigningKey() (keychain.KeyDescriptor, error) {
	return c.keyRing.DeriveNextKey(musigSigningKeyFamily)
}

func (c *clientLndWallet) WatchTaprootScript(tapscript *waddrmgr.Tapscript) (
	btcutil.Address, error) {

	// Import the script into the wallet to get the address.
	addr, err := c.wallet.ImportTaprootScript(
		waddrmgr.KeyScopeBIP0086, tapscript,
	)
	if err != nil {
		return nil, err
	}

	return addr.Address(), nil
}

func (c *clientLndWallet) NextAddress() (btcutil.Address, error) {
	return c.wallet.NewAddress(
		lnwallet.TaprootPubkey, false, lnwallet.DefaultAccountName,
	)
}

var _ ClientWallet = (*clientLndWallet)(nil)
