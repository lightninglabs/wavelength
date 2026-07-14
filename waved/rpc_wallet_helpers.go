package waved

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/wallet"
)

// NewWalletAddress returns a fresh backing-wallet receive address for wallet
// RPC facade code that needs an internal cooperative-exit destination.
func (r *RPCServer) NewWalletAddress(ctx context.Context) (string, error) {
	return r.server.NewWalletAddress(ctx)
}

// ListWalletUnspent returns backing-wallet UTXOs in the requested confirmation
// range for wallet RPC facade preflight and activity-correlation checks.
func (r *RPCServer) ListWalletUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*wallet.Utxo, error) {

	return r.server.ListWalletUnspent(ctx, minConfs, maxConfs)
}

// ListActiveBoardingAddresses returns the persisted boarding-address registry
// to in-process wallet facades. It is intentionally not a public daemon RPC;
// callers use it to correlate zero-conf wallet UTXOs with their stable
// deposit-<address> activity IDs.
func (r *RPCServer) ListActiveBoardingAddresses(ctx context.Context) ([]string,
	error) {

	if r == nil || r.server == nil || r.server.boardingSweepStore == nil {
		return nil, fmt.Errorf("boarding address store unavailable")
	}

	addresses, err := r.server.boardingSweepStore.
		ListAllBoardingAddresses(
			ctx,
		)
	if err != nil {
		return nil, fmt.Errorf("list boarding addresses: %w", err)
	}

	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if address == nil || address.Address == nil {
			continue
		}

		result = append(result, address.Address.String())
	}

	return result, nil
}

// ListUnconfirmedBoardingUTXOs returns zero-conf outputs whose scripts belong
// to the persisted boarding-address registry. ListWalletUnspent intentionally
// removes these outputs for fee-input callers, so activity correlation needs
// this narrowly scoped inverse view.
func (r *RPCServer) ListUnconfirmedBoardingUTXOs(ctx context.Context) (
	[]*wallet.Utxo, error) {

	if r == nil || r.server == nil {
		return nil, fmt.Errorf("wallet server unavailable")
	}

	utxos, err := r.server.listBackingWalletUnspent(ctx, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("list zero-conf wallet outputs: %w", err)
	}
	boardingScripts, err := r.server.boardingScripts(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshot boarding scripts: %w", err)
	}

	result := make([]*wallet.Utxo, 0, len(utxos))
	for _, utxo := range utxos {
		if utxo == nil || utxo.Confirmations != 0 {
			continue
		}
		if _, ok := boardingScripts[string(utxo.PkScript)]; !ok {
			continue
		}

		result = append(result, utxo)
	}

	return result, nil
}
