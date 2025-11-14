package stores

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/ark/lib/types"
	"github.com/lightningnetwork/lnd/lnwallet"
)

type BoardingStore interface {
	Register(addr *types.BoardingAddress) error

	RegisterUTXOs(addr btcutil.Address, utxos []*lnwallet.Utxo) error

	ListAddresses() ([]btcutil.Address, error)

	ListUTXOs() ([]*types.BoardingUTXO, error)

	ListExpiredUTXOs() ([]*types.BoardingUTXO, error)

	FindBoardingUTXO(outpoint *wire.OutPoint) (*types.BoardingUTXO, error)
}

type VTXOStore interface {
	// GetVTXO retrieves a VTXO by its outpoint
	GetVTXO(outpoint *wire.OutPoint) (*types.ServerVTXO, error)

	// AddVTXOs adds new VTXOs to the store (called on successful batch completion)
	AddVTXOs(vtxos []*types.ServerVTXO) error

	// RemoveVTXOs removes VTXOs from the store (called on successful forfeit)
	RemoveVTXOs(outpoints []*wire.OutPoint) error

	// ListVTXOs returns all VTXOs currently stored
	ListVTXOs() ([]*types.ServerVTXO, error)
}
