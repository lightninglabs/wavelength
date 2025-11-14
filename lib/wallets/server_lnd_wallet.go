package wallets

import (
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

const (
	// operatorKeyFamily is the key family for the main operator account.
	operatorKeyFamily = 42069

	// operatorKeyIndex is the key index for the main operator key.
	operatorKeyIndex = 0

	operatorSweepKeyFamily        = 42070
	operatorBatchSigningKeyFamily = 42071
	operatorForfeitKeyFamily      = 42072
	operatorForfeitKeyIndex       = 0
)

var (
	// operatorKeyLocator is a static key locator used for the main
	// operator account.
	operatorKeyLocator = keychain.KeyLocator{
		Family: operatorKeyFamily,
		Index:  operatorKeyIndex,
	}

	// operatorForfeitKeyLocator is a static key locator used for the
	// forfeit address.
	operatorForfeitKeyLocator = keychain.KeyLocator{
		Family: operatorForfeitKeyFamily,
		Index:  operatorForfeitKeyIndex,
	}
)

type operatorLNDWallet struct {
	chainParams *chaincfg.Params
	keyRing     keychain.SecretKeyRing
	wallet      lnwallet.WalletController
	input.Signer
}

func NewOperatorLNDWallet(chainParams *chaincfg.Params,
	keyRing keychain.SecretKeyRing, signer input.Signer,
	wallet lnwallet.WalletController) OperatorWallet {

	return &operatorLNDWallet{
		chainParams: chainParams,
		keyRing:     keyRing,
		wallet:      wallet,
		Signer:      signer,
	}
}

func (o *operatorLNDWallet) MainOperatorKey() (*keychain.KeyDescriptor, error) {
	keyDesc, err := o.keyRing.DeriveKey(operatorKeyLocator)
	if err != nil {
		return nil, err
	}

	return &keyDesc, nil
}

func (o *operatorLNDWallet) NewAddress() (btcutil.Address, error) {
	return o.wallet.NewAddress(
		lnwallet.TaprootPubkey, true, lnwallet.DefaultAccountName,
	)
}

func (o *operatorLNDWallet) NewTaprootAddress() (*btcutil.AddressTaproot, error) {
	addr, err := o.wallet.NewAddress(
		lnwallet.TaprootPubkey, true, lnwallet.DefaultAccountName,
	)
	if err != nil {
		return nil, err
	}

	tapAddr, ok := addr.(*btcutil.AddressTaproot)
	if !ok {
		return nil, fmt.Errorf("expected btcutil.AddressTaproot")
	}

	return tapAddr, nil
}

func (o *operatorLNDWallet) ListAvailableUTXOs() ([]*lnwallet.Utxo, error) {
	return o.wallet.ListUnspentWitness(1, math.MaxInt32, "")
}

func (o *operatorLNDWallet) NewSweepKey() (*keychain.KeyDescriptor, error) {
	keyDesc, err := o.keyRing.DeriveNextKey(operatorSweepKeyFamily)
	if err != nil {
		return nil, err
	}

	return &keyDesc, nil
}

func (o *operatorLNDWallet) NewBatchSignerKey() (*keychain.KeyDescriptor, error) {
	keyDesc, err := o.keyRing.DeriveNextKey(operatorBatchSigningKeyFamily)
	if err != nil {
		return nil, err
	}

	return &keyDesc, nil
}

func (o *operatorLNDWallet) GetForfeitAddress() (btcutil.Address, error) {
	keyDesc, err := o.keyRing.DeriveKey(operatorForfeitKeyLocator)
	if err != nil {
		return nil, fmt.Errorf("failed to derive forfeit key: %w", err)
	}

	pubkeyHash := btcutil.Hash160(keyDesc.PubKey.SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubkeyHash, o.chainParams)
	if err != nil {
		return nil, fmt.Errorf("failed to create forfeit address: %w", err)
	}

	return addr, nil
}

var _ OperatorWallet = (*operatorLNDWallet)(nil)
