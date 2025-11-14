package stores

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/ark/lib/types"
	"github.com/lightningnetwork/lnd/lnwallet"
)

type InMemoryBoardingStore struct {
	chainParams *chaincfg.Params
	scripts     map[string]*types.BoardingAddress
	utxos       map[string][]*lnwallet.Utxo
}

func NewInMemoryBoardingStore(params *chaincfg.Params) *InMemoryBoardingStore {
	return &InMemoryBoardingStore{
		chainParams: params,
		scripts:     make(map[string]*types.BoardingAddress),
		utxos:       make(map[string][]*lnwallet.Utxo),
	}
}

func (i *InMemoryBoardingStore) ListAddresses() ([]btcutil.Address, error) {
	var addrs []btcutil.Address
	for addrStr := range i.scripts {
		addr, err := btcutil.DecodeAddress(addrStr, i.chainParams)
		if err != nil {
			return nil, err
		}
		addrs = append(addrs, addr)
	}
	return addrs, nil
}

func (i *InMemoryBoardingStore) Register(addr *types.BoardingAddress) error {
	i.scripts[addr.Address.EncodeAddress()] = addr
	i.utxos[addr.Address.EncodeAddress()] = make([]*lnwallet.Utxo, 0)

	return nil
}

func (i *InMemoryBoardingStore) RegisterUTXOs(addr btcutil.Address,
	utxos []*lnwallet.Utxo) error {

	_, ok := i.scripts[addr.EncodeAddress()]
	if !ok {
		return fmt.Errorf("no such address")
	}

	i.utxos[addr.EncodeAddress()] = utxos

	return nil
}

func (i *InMemoryBoardingStore) ListUTXOs() ([]*types.BoardingUTXO, error) {
	var allUTXOs []*types.BoardingUTXO
	for addrDesc, utxoList := range i.utxos {
		addr, ok := i.scripts[addrDesc]
		if !ok {
			return nil, fmt.Errorf("no such address")
		}

		for _, utxo := range utxoList {
			allUTXOs = append(allUTXOs, &types.BoardingUTXO{
				Address: addr,
				UTXO:    utxo,
			})
		}
	}
	return allUTXOs, nil
}

func (i *InMemoryBoardingStore) FindBoardingUTXO(outpoint *wire.OutPoint) (*types.BoardingUTXO, error) {
	for addrDesc, utxoList := range i.utxos {
		addr, ok := i.scripts[addrDesc]
		if !ok {
			return nil, fmt.Errorf("no such address")
		}

		for _, utxo := range utxoList {
			if utxo.OutPoint == *outpoint {
				return &types.BoardingUTXO{
					Address: addr,
					UTXO:    utxo,
				}, nil
			}
		}
	}
	return nil, fmt.Errorf("no such UTXO")
}

func (i *InMemoryBoardingStore) ListExpiredUTXOs() ([]*types.BoardingUTXO, error) {
	var expiredUTXOs []*types.BoardingUTXO
	for addrDesc, utxoList := range i.utxos {
		addr, ok := i.scripts[addrDesc]
		if !ok {
			return nil, fmt.Errorf("no such address")
		}

		for _, utxo := range utxoList {
			if utxo.Confirmations < int64(addr.ExitDelay) {
				continue
			}
			expiredUTXOs = append(expiredUTXOs, &types.BoardingUTXO{
				Address: addr,
				UTXO:    utxo,
			})
		}
	}
	return expiredUTXOs, nil
}

var _ BoardingStore = (*InMemoryBoardingStore)(nil)
