package waved

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/lndbackend"
	"github.com/lightninglabs/wavelength/wallet"
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
		services := s.lnd.UnsafeFromSome().Services()

		addr, err := services.WalletKit.NextAddr(
			ctx, lnwallet.DefaultAccountName,
			walletrpc.AddressType_TAPROOT_PUBKEY, false,
		)
		if err != nil {
			return "", fmt.Errorf("LND NextAddr: %w", err)
		}

		return addr.String(), nil
	}

	if s.lwWallet.IsSome() {
		addr, err := s.lwWallet.UnsafeFromSome().NewAddress(ctx)
		if err != nil {
			return "", fmt.Errorf("lightweight wallet new "+
				"address: %w", err)
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

// ListWalletUnspent returns confirmed backing-wallet UTXOs for tests
// and harness helpers, regardless of the active wallet backend. The
// result excludes outputs at imported boarding scripts so callers see
// only the wallet's own HD-keyed UTXOs (the candidates that can fund
// CPFP fee inputs, normal sends, etc.). Boarding outputs are surfaced
// through the dedicated boarding-balance APIs instead.
//
// The exclusion matters most on the lwwallet / btcwbackend paths,
// where the underlying btcwallet credit-tracking pipeline removes
// spent boarding outputs asynchronously: without the filter the
// preflight gates that count "available fee inputs" can briefly
// observe a stale boarding output post-spend. The same filter is
// applied uniformly on the LND path so all backends report the same
// wallet view.
func (s *Server) ListWalletUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*wallet.Utxo, error) {

	if !s.isWalletReady() {
		return nil, fmt.Errorf("wallet is not ready")
	}

	utxos, err := s.listBackingWalletUnspent(ctx, minConfs, maxConfs)
	if err != nil {
		return nil, err
	}

	boardingScripts, err := s.boardingScripts(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshot boarding scripts: %w", err)
	}

	return filterBoardingScripts(utxos, boardingScripts), nil
}

// listBackingWalletUnspent dispatches to the active wallet backend's
// raw unspent-output query without any filtering. Split from
// ListWalletUnspent so the dispatch is independently testable and the
// post-filter step is a one-liner at the public entry point.
func (s *Server) listBackingWalletUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*wallet.Utxo, error) {

	if s.lnd.IsSome() {
		services := s.lnd.UnsafeFromSome().Services()
		backend := lndbackend.NewBoardingBackend(
			services.WalletKit, services.ChainKit,
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
			return nil, fmt.Errorf("lightweight wallet list "+
				"unspent: %w", err)
		}

		return walletUtxosFromLNWallet(lnUtxos), nil
	}

	if s.btcwWallet.IsSome() {
		btcw := s.btcwWallet.UnsafeFromSome()
		lnUtxos, err := btcw.ListUnspentWitness(
			minConfs, maxConfs,
		)
		if err != nil {
			return nil, fmt.Errorf("btcwallet list unspent: %w",
				err)
		}

		return walletUtxosFromLNWallet(lnUtxos), nil
	}

	return nil, fmt.Errorf("wallet backend is not initialized")
}

// boardingScripts returns the set of persisted boarding-address
// pkScripts as a set keyed by the raw script bytes. Used to filter
// boarding outputs out of the backing-wallet view (see
// ListWalletUnspent). The boarding store is the authoritative source
// because in-memory tracking on lwwallet / btcwbackend can drift
// across restarts before the wallet actor re-imports scripts.
func (s *Server) boardingScripts(ctx context.Context) (
	map[string]struct{}, error) {

	boardingStore := s.newBoardingStore()

	addrs, err := boardingStore.ListAllBoardingAddresses(ctx)
	if err != nil {
		return nil, fmt.Errorf("list boarding addresses: %w", err)
	}

	scripts := make(map[string]struct{}, len(addrs))
	for _, addr := range addrs {
		if addr == nil || addr.Address == nil {
			continue
		}

		pkScript, err := txscript.PayToAddrScript(addr.Address)
		if err != nil {
			// A malformed persisted boarding address can't
			// match any wallet UTXO anyway, so skip it
			// rather than failing the whole query.
			continue
		}

		scripts[string(pkScript)] = struct{}{}
	}

	return scripts, nil
}

// filterBoardingScripts returns the subset of utxos whose pkScript is
// not present in the supplied boarding-script set. Pure helper split
// from ListWalletUnspent so the policy is unit-testable without a
// running wallet backend.
func filterBoardingScripts(utxos []*wallet.Utxo,
	boardingScripts map[string]struct{}) []*wallet.Utxo {

	if len(boardingScripts) == 0 {
		return utxos
	}

	// Allocate a fresh slice rather than re-using utxos' backing
	// array. utxos[:0] is technically safe today (callers do not
	// reuse utxos after this call) but it silently overwrites
	// elements in-place, which is a footgun if a future caller
	// reads utxos after filtering or inlines this helper.
	filtered := make([]*wallet.Utxo, 0, len(utxos))
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		if _, isBoarding := boardingScripts[string(
			utxo.PkScript,
		)]; isBoarding {

			continue
		}

		filtered = append(filtered, utxo)
	}

	return filtered
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
