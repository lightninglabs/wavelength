package waved

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/lndbackend"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

// lndAccountLister is the slice of lndclient.WalletKitClient that account
// validation needs.
type lndAccountLister interface {
	// ListAccounts returns the wallet accounts matching the optional name
	// and address type filters.
	ListAccounts(ctx context.Context, name string,
		addressType walletrpc.AddressType) ([]*walletrpc.Account, error)
}

// lndRemoteSigning reports whether lnd signs through a remote signer.
//
// A watch-only wallet holds no private keys, so every account it lists is
// watch-only, including the built-in default taproot account. A wallet with
// local keys always reports that account as signable, which makes it a
// reliable marker of the signing mode without a configuration flag. Anything
// other than exactly one default account is an error, so an empty or
// ambiguous answer never selects remote signing.
func lndRemoteSigning(ctx context.Context,
	lister lndAccountLister) (bool, error) {

	accounts, err := lister.ListAccounts(
		ctx, lndbackend.DefaultWalletAccount,
		walletrpc.AddressType_TAPROOT_PUBKEY,
	)
	if err != nil {
		return false, fmt.Errorf("list the default taproot account to "+
			"detect remote signing: %w", err)
	}
	if len(accounts) != 1 {
		return false, fmt.Errorf("expected exactly one default "+
			"taproot account to detect remote signing, got %d",
			len(accounts))
	}

	return accounts[0].GetWatchOnly(), nil
}

// checkLndAccount returns why this daemon must refuse the configured lnd
// account, or nil when lnd can sign for it.
//
// A watch-only account is usable only on a remote-signing node, where the
// signer holds the keys and lnd forwards each signing request to it, exactly
// as it already does for the default account. lnd signs for an imported
// account by sending the signer the derivation path it computes from the
// account's extended public key, and the signer derives and checks the key
// itself. That is why the xpub and derivation path must be present, and why
// nothing else about the account is verified here. On a node with local keys
// a watch-only account stays rejected, as lnd has no key to sign with.
//
// This never creates an account: a missing one is an error.
func checkLndAccount(ctx context.Context, lister lndAccountLister,
	account string) error {

	accounts, err := lister.ListAccounts(
		ctx, account, walletrpc.AddressType_TAPROOT_PUBKEY,
	)
	if err != nil || len(accounts) == 0 {
		return fmt.Errorf("lnd wallet account %q not found under the "+
			"taproot key scope: this daemon derives taproot "+
			"addresses, and lnd resolves an account name within "+
			"the key scope the requested address type implies, so "+
			"the account must be created taproot-scoped (lookup "+
			"error: %v)", account, err)
	}

	acct := accounts[0]
	if !acct.GetWatchOnly() {
		return nil
	}

	// The default account is consulted only for a watch-only account, so
	// a node with local keys never depends on that extra lookup.
	remote, err := lndRemoteSigning(ctx, lister)
	if err != nil {
		return err
	}
	if !remote {
		return fmt.Errorf("lnd wallet account %q is watch-only, so "+
			"lnd cannot sign for it: this lnd has local keys and "+
			"no remote signer, and coin selection would succeed "+
			"while signing would fail after the inputs were "+
			"already leased", account)
	}

	if acct.GetName() != account ||
		acct.GetAddressType() != walletrpc.AddressType_TAPROOT_PUBKEY ||
		acct.GetExtendedPublicKey() == "" ||
		acct.GetDerivationPath() == "" {
		return fmt.Errorf("lnd wallet account %q is a watch-only "+
			"account but is not a taproot account reporting an "+
			"extended public key and derivation path, which lnd "+
			"needs to sign for it through the remote signer",
			account)
	}

	return nil
}
