package waved

import (
	"fmt"

	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lndbackend"
	"github.com/lightninglabs/wavelength/unroll"
)

// newBoardingStore returns a concrete boarding store for RPC-only direct
// reads and status updates. The wallet actor receives an instance of this
// store via NewArk and consumes it through the wallet.BoardingSweepStore
// interface.
func (s *Server) newBoardingStore() *db.BoardingWalletStore {
	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(),
		s.subLogger(db.Subsystem),
	)

	return dbStore.NewBoardingStore(s.chainParams, s.clk)
}

// newSweepWallet returns the wallet adapter used to sign timeout-path sweep
// inputs and derive sweep destination scripts. The concrete adapter
// (lndUnrollWallet / lwUnrollWallet / btcwUnrollWallet) is structurally
// compatible with both unroll.SweepWallet (used by the unilateral-exit
// runtime) and wallet.SweepSigner (used by the boarding-sweep flow inside
// the wallet actor).
func (s *Server) newSweepWallet() (unroll.SweepWallet, error) {
	switch s.cfg.Wallet.Type {
	case WalletTypeLnd:
		if !s.lnd.IsSome() {
			return nil, fmt.Errorf("lnd wallet not initialized")
		}

		services := s.lnd.UnsafeFromSome().Services()
		clientWallet := lndbackend.NewClientWallet(
			services.Signer, services.WalletKit,
		)
		boardingBackend := lndbackend.NewBoardingBackend(
			services.WalletKit, services.ChainKit,
		)

		return &lndUnrollWallet{
			ClientWallet:    clientWallet,
			boardingBackend: boardingBackend,
		}, nil

	case WalletTypeLwwallet:
		if !s.lwWallet.IsSome() {
			return nil, fmt.Errorf("lightweight wallet not " +
				"initialized")
		}

		return &lwUnrollWallet{
			Wallet: s.lwWallet.UnsafeFromSome(),
		}, nil

	case WalletTypeBtcwallet:
		if !s.btcwWallet.IsSome() {
			return nil, fmt.Errorf("btcwallet not initialized")
		}

		return &btcwUnrollWallet{
			Wallet: s.btcwWallet.UnsafeFromSome(),
		}, nil

	default:
		return nil, fmt.Errorf("unknown wallet type %q",
			s.cfg.Wallet.Type)
	}
}
