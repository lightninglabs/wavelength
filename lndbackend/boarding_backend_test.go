package lndbackend

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// fakeWalletKit stubs the walletKit ListUnspent call, recording the
// request that the supplied functional options build so tests can assert
// which account filter (if any) each backend method applies. The embedded
// interface panics on any other method, which is fine: these tests only
// exercise UTXO enumeration.
type fakeWalletKit struct {
	lndclient.WalletKitClient

	lastReq *walletrpc.ListUnspentRequest
	utxos   []*lnwallet.Utxo
}

// ListUnspent records the effective request and returns the configured
// UTXO set.
func (f *fakeWalletKit) ListUnspent(_ context.Context, minConfs, maxConfs int32,
	opts ...lndclient.ListUnspentOption) ([]*lnwallet.Utxo, error) {

	req := &walletrpc.ListUnspentRequest{
		MinConfs: minConfs,
		MaxConfs: maxConfs,
	}
	for _, opt := range opts {
		opt(req)
	}
	f.lastReq = req

	return f.utxos, nil
}

// TestListUnspentAccountScoping verifies that the general ListUnspent
// enumeration spans all accounts while ListUnspentWalletAccount pins the
// query to the backend's own account. The distinction is what keeps imported
// watch-only script outputs (boarding/exit scripts) out of CPFP fee-input
// selection: those coins list fine but LND's FinalizePsbt cannot sign
// them, so offering one as a fee input fails the child with "PSBT is not
// finalizable".
func TestListUnspentAccountScoping(t *testing.T) {
	t.Parallel()

	utxo := &lnwallet.Utxo{
		AddressType:   lnwallet.TaprootPubkey,
		Value:         btcutil.Amount(50_000),
		Confirmations: 3,
		PkScript: []byte{
			0x51,
			0x20,
		},
		OutPoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x01,
			},
			Index: 1,
		},
	}

	walletKit := &fakeWalletKit{utxos: []*lnwallet.Utxo{utxo}}
	backend := NewBoardingBackend(walletKit, nil)

	// The unfiltered enumeration must not set an account: callers like
	// the wallet actor and boarding detection rely on seeing imported
	// watch-only outputs.
	utxos, err := backend.ListUnspent(t.Context(), 1, 100)
	require.NoError(t, err)
	require.Empty(t, walletKit.lastReq.Account)
	require.Equal(t, int32(1), walletKit.lastReq.MinConfs)
	require.Equal(t, int32(100), walletKit.lastReq.MaxConfs)

	// The converted UTXO carries the outpoint, script, and amount.
	require.Len(t, utxos, 1)
	require.Equal(t, utxo.OutPoint, utxos[0].Outpoint)
	require.Equal(t, utxo.PkScript, utxos[0].PkScript)
	require.Equal(t, utxo.Value, utxos[0].Amount)
	require.Equal(t, int32(3), utxos[0].Confirmations)

	// The fee-input enumeration pins the wallet's account so only coins
	// the wallet can unilaterally sign are offered. Without an explicit
	// account that is LND's default one.
	utxos, err = backend.ListUnspentWalletAccount(t.Context(), 1, 100)
	require.NoError(t, err)
	require.Equal(t, DefaultWalletAccount, walletKit.lastReq.Account)
	require.Len(t, utxos, 1)
	require.True(t, backend.IsDefaultAccount())
}

// TestListUnspentCustomAccount verifies that a backend pinned to a named
// account only ever offers that account's coins for spending, while still
// enumerating every account when asked for the unfiltered view. This is what
// stops a daemon sharing an LND node from funding its transactions out of
// another daemon's balance.
func TestListUnspentCustomAccount(t *testing.T) {
	t.Parallel()

	const account = "swapd"

	walletKit := &fakeWalletKit{}
	backend := NewBoardingBackend(walletKit, nil, WithAccount(account))

	require.Equal(t, account, backend.Account())
	require.False(t, backend.IsDefaultAccount())

	// Spending enumeration is confined to the configured account.
	_, err := backend.ListUnspentWalletAccount(t.Context(), 1, 100)
	require.NoError(t, err)
	require.Equal(t, account, walletKit.lastReq.Account)

	// Observation is deliberately not: imported boarding scripts live in
	// LND's watch-only account and must stay visible.
	_, err = backend.ListUnspent(t.Context(), 1, 100)
	require.NoError(t, err)
	require.Empty(t, walletKit.lastReq.Account)
}

// TestWithAccountEmptyKeepsDefault verifies that an unset account config leaves
// the backend on LND's default account, so deployments that never configure
// accounts keep their existing behaviour.
func TestWithAccountEmptyKeepsDefault(t *testing.T) {
	t.Parallel()

	backend := NewBoardingBackend(&fakeWalletKit{}, nil, WithAccount(""))
	require.Equal(t, DefaultWalletAccount, backend.Account())
	require.True(t, backend.IsDefaultAccount())
}

// TestListUnspentObservationStaysUnscoped is a regression guard for the split
// between observing and spending.
//
// Boarding scripts are imported into LND's watch-only account, which belongs
// to no wallet account, so an account filter excludes them. Callers that exist
// to see pending deposits — the unconfirmed boarding balance and the boarding
// activity view — go through the unfiltered enumeration for exactly that
// reason. Narrowing it would empty both with no error and no log line, and
// would do so even with no account configured, since LND treats an empty
// account as "every account" but "default" as a real filter.
func TestListUnspentObservationStaysUnscoped(t *testing.T) {
	t.Parallel()

	for _, account := range []string{"", DefaultWalletAccount, "swapd"} {
		walletKit := &fakeWalletKit{}
		backend := NewBoardingBackend(
			walletKit, nil, WithAccount(account),
		)

		_, err := backend.ListUnspent(t.Context(), 0, 100)
		require.NoError(t, err)
		require.Empty(
			t, walletKit.lastReq.Account, "observation must not "+
				"be filtered by account, configured account %q",
			account,
		)
	}
}
