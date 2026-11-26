//go:build itest

package waved

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/wallet"
)

// FinalizeTestWalletPSBT lets an integration harness retain a valid competing
// spend before the fee broadcaster leases its inputs. It never publishes it.
func (s *Server) FinalizeTestWalletPSBT(ctx context.Context, packet []byte) (
	*wire.MsgTx, error) {

	signer, err := s.newSweepWallet()
	if err != nil {
		return nil, err
	}
	sweeper, ok := signer.(wallet.WalletBackingSweeper)
	if !ok {
		return nil, fmt.Errorf("wallet cannot finalize test PSBT")
	}

	return sweeper.FinalizePsbt(ctx, packet)
}
