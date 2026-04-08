package darepod

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// NewWalletAddress returns a fresh backing-wallet receive address for tests
// and harness helpers, regardless of the active wallet backend.
func (s *Server) NewWalletAddress(ctx context.Context) (string, error) {
	if !s.isWalletReady() {
		return "", fmt.Errorf("wallet is not ready")
	}

	if s.lnd.IsSome() {
		lndSvc := s.lnd.UnsafeFromSome()

		addr, err := lndSvc.WalletKit.NextAddr(
			ctx, lnwallet.DefaultAccountName,
			walletrpc.AddressType_TAPROOT_PUBKEY, true,
		)
		if err != nil {
			return "", fmt.Errorf("LND NextAddr: %w", err)
		}

		return addr.String(), nil
	}

	if s.lwWallet.IsSome() {
		addr, err := s.lwWallet.UnsafeFromSome().NewAddress(ctx)
		if err != nil {
			return "", fmt.Errorf("lightweight wallet new address: %w", err)
		}

		return addr.String(), nil
	}

	if s.btcwWallet.IsSome() {
		addr, err := s.btcwWallet.UnsafeFromSome().NewAddress(ctx)
		if err != nil {
			return "", fmt.Errorf("btcwallet new address: %w", err)
		}

		return addr.String(), nil
	}

	return "", fmt.Errorf("wallet backend is not initialized")
}

// ListWalletUnspent returns confirmed backing-wallet UTXOs for tests and
// harness helpers, regardless of the active wallet backend.
func (s *Server) ListWalletUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*wallet.Utxo, error) {

	if !s.isWalletReady() {
		return nil, fmt.Errorf("wallet is not ready")
	}

	if s.lnd.IsSome() {
		lndSvc := s.lnd.UnsafeFromSome()
		backend := lndbackend.NewBoardingBackend(
			lndSvc.WalletKit, lndSvc.ChainKit,
		)

		utxos, err := backend.ListUnspent(ctx, minConfs, maxConfs)
		if err != nil {
			return nil, fmt.Errorf("LND list unspent: %w", err)
		}

		return utxos, nil
	}

	if s.lwWallet.IsSome() {
		lnUtxos, err := s.lwWallet.UnsafeFromSome().ListUnspentWitness(
			minConfs, maxConfs,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"lightweight wallet list unspent: %w", err,
			)
		}

		return walletUtxosFromLNWallet(lnUtxos), nil
	}

	if s.btcwWallet.IsSome() {
		lnUtxos, err := s.btcwWallet.UnsafeFromSome().ListUnspentWitness(
			minConfs, maxConfs,
		)
		if err != nil {
			return nil, fmt.Errorf("btcwallet list unspent: %w", err)
		}

		return walletUtxosFromLNWallet(lnUtxos), nil
	}

	return nil, fmt.Errorf("wallet backend is not initialized")
}

// walletUtxosFromLNWallet converts lnwallet UTXOs into the simplified wallet
// package representation used by harness helpers.
func walletUtxosFromLNWallet(lnUtxos []*lnwallet.Utxo) []*wallet.Utxo {
	result := make([]*wallet.Utxo, 0, len(lnUtxos))
	for _, utxo := range lnUtxos {
		result = append(result, &wallet.Utxo{
			Outpoint:      utxo.OutPoint,
			PkScript:      utxo.PkScript,
			Amount:        utxo.Value,
			Confirmations: int32(utxo.Confirmations),
		})
	}

	return result
}
