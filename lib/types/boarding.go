package types

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

type BoardingAddress struct {
	Address     btcutil.Address
	Tapscript   *waddrmgr.Tapscript
	KeyDesc     keychain.KeyDescriptor
	OperatorKey *btcec.PublicKey
	ExitDelay   uint32
}

type BoardingUTXO struct {
	Address *BoardingAddress
	UTXO    *lnwallet.Utxo
}

func (u *BoardingUTXO) Expired() bool {
	return u.UTXO.Confirmations >= int64(u.Address.ExitDelay)
}
