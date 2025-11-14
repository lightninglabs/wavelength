package wallets

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// ClientWallet is an abstraction over a wallet that the client will need.
type ClientWallet interface {
	input.Signer
	NextBoardingKey() (keychain.KeyDescriptor, error)
	NextVTXOKey() (keychain.KeyDescriptor, error)
	NextMusig2SigningKey() (keychain.KeyDescriptor, error)
	WatchTaprootScript(*waddrmgr.Tapscript) (btcutil.Address, error)
	GetUTXOsForAddress(btcutil.Address) ([]*lnwallet.Utxo, error)
	NextAddress() (btcutil.Address, error)
}

// OperatorWallet is an abstraction over a wallet that the operator will need.
type OperatorWallet interface {
	input.Signer
	MainOperatorKey() (*keychain.KeyDescriptor, error)
	NewSweepKey() (*keychain.KeyDescriptor, error)
	NewBatchSignerKey() (*keychain.KeyDescriptor, error)
	NewAddress() (btcutil.Address, error)
	NewTaprootAddress() (*btcutil.AddressTaproot, error)
	ListAvailableUTXOs() ([]*lnwallet.Utxo, error)
	GetForfeitAddress() (btcutil.Address, error)

	ComputeInputScript(*wire.MsgTx, *input.SignDescriptor) (*input.Script,
		error)
}
