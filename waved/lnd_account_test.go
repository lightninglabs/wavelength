package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
)

// taprootV1 is the taproot address type the account check requires.
const taprootV1 = walletrpc.AddressType_TAPROOT_PUBKEY

// segwitV0 is a non-taproot address type used to exercise the scope check.
const segwitV0 = walletrpc.AddressType_WITNESS_PUBKEY_HASH

// fakeAccountLister serves canned ListAccounts answers keyed by account name
// and records every name it was asked about.
type fakeAccountLister struct {
	// answers maps an account name to the accounts returned for it.
	answers map[string][]*walletrpc.Account

	// errs maps an account name to the error returned for it.
	errs map[string]error

	// asked records the names looked up, in order.
	asked []string
}

// ListAccounts implements lndAccountLister.
func (f *fakeAccountLister) ListAccounts(_ context.Context, name string,
	addressType walletrpc.AddressType) ([]*walletrpc.Account, error) {

	f.asked = append(f.asked, name)
	if addressType != walletrpc.AddressType_TAPROOT_PUBKEY {
		return nil, errors.New("unexpected address type")
	}
	if err, ok := f.errs[name]; ok {
		return nil, err
	}

	return f.answers[name], nil
}

// TestCheckLndAccount pins which configured lnd accounts the daemon accepts,
// in particular that a watch-only account is accepted only on a remote-signing
// node and that an unclear default account lookup fails closed.
func TestCheckLndAccount(t *testing.T) {
	t.Parallel()

	const name = "swapd"

	// taproot builds a taproot account named name.
	taproot := func(watchOnly bool, xpub, path string) *walletrpc.Account {
		return &walletrpc.Account{
			Name:              name,
			AddressType:       walletrpc.AddressType_TAPROOT_PUBKEY,
			WatchOnly:         watchOnly,
			ExtendedPublicKey: xpub,
			DerivationPath:    path,
		}
	}
	def := func(watchOnly bool) *walletrpc.Account {
		return &walletrpc.Account{
			Name:        "default",
			AddressType: walletrpc.AddressType_TAPROOT_PUBKEY,
			WatchOnly:   watchOnly,
		}
	}

	const (
		xpub = "xpub-test"
		path = "m/86'/0'/1'"
	)

	tests := []struct {
		name string

		answers map[string][]*walletrpc.Account
		errs    map[string]error

		// wantErr is a substring of the expected error, empty when the
		// account must be accepted.
		wantErr string

		// wantAsked is the exact lookup sequence.
		wantAsked []string
	}{
		{
			name: "local node with signable account",
			answers: map[string][]*walletrpc.Account{
				name: {
					taproot(false, xpub, path),
				},
			},
			wantAsked: []string{
				name,
			},
		},
		{
			name: "local node with watch-only account",
			answers: map[string][]*walletrpc.Account{
				name: {
					taproot(true, xpub, path),
				},
				"default": {
					def(false),
				},
			},
			wantErr: "lnd has local keys and no remote signer",
			wantAsked: []string{
				name,
				"default",
			},
		},
		{
			name: "remote node with watch-only account",
			answers: map[string][]*walletrpc.Account{
				name: {
					taproot(true, xpub, path),
				},
				"default": {
					def(true),
				},
			},
			wantAsked: []string{
				name,
				"default",
			},
		},
		{
			name: "remote node with account missing xpub",
			answers: map[string][]*walletrpc.Account{
				name: {
					taproot(true, "", path),
				},
				"default": {
					def(true),
				},
			},
			wantErr: "extended public key and derivation path",
			wantAsked: []string{
				name,
				"default",
			},
		},
		{
			name: "remote node with account missing path",
			answers: map[string][]*walletrpc.Account{
				name: {
					taproot(true, xpub, ""),
				},
				"default": {
					def(true),
				},
			},
			wantErr: "extended public key and derivation path",
			wantAsked: []string{
				name,
				"default",
			},
		},
		{
			name: "remote node with account of another name",
			answers: map[string][]*walletrpc.Account{
				name: {{
					Name:              "other",
					AddressType:       taprootV1,
					WatchOnly:         true,
					ExtendedPublicKey: xpub,
					DerivationPath:    path,
				}},
				"default": {
					def(true),
				},
			},
			wantErr: "extended public key and derivation path",
			wantAsked: []string{
				name,
				"default",
			},
		},
		{
			name: "remote node with wrong address type",
			answers: map[string][]*walletrpc.Account{
				name: {{
					Name:              name,
					AddressType:       segwitV0,
					WatchOnly:         true,
					ExtendedPublicKey: xpub,
					DerivationPath:    path,
				}},
				"default": {
					def(true),
				},
			},
			wantErr: "extended public key and derivation path",
			wantAsked: []string{
				name,
				"default",
			},
		},
		{
			name:    "account missing",
			wantErr: "not found under the taproot key scope",
			wantAsked: []string{
				name,
			},
		},
		{
			name: "account lookup error",
			errs: map[string]error{
				name: errors.New("rpc down"),
			},
			wantErr: "rpc down",
			wantAsked: []string{
				name,
			},
		},
		{
			name: "default lookup error",
			answers: map[string][]*walletrpc.Account{
				name: {
					taproot(true, xpub, path),
				},
			},
			errs: map[string]error{
				"default": errors.New("rpc down"),
			},
			wantErr: "detect remote signing: rpc down",
			wantAsked: []string{
				name,
				"default",
			},
		},
		{
			name: "default lookup empty",
			answers: map[string][]*walletrpc.Account{
				name: {
					taproot(true, xpub, path),
				},
			},
			wantErr: "exactly one default taproot account",
			wantAsked: []string{
				name,
				"default",
			},
		},
		{
			name: "default lookup ambiguous",
			answers: map[string][]*walletrpc.Account{
				name: {
					taproot(true, xpub, path),
				},
				"default": {
					def(true),
					def(true),
				},
			},
			wantErr: "exactly one default taproot account",
			wantAsked: []string{
				name,
				"default",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			lister := &fakeAccountLister{
				answers: test.answers,
				errs:    test.errs,
			}

			err := checkLndAccount(t.Context(), lister, name)
			if test.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, test.wantErr)
			}

			require.Equal(t, test.wantAsked, lister.asked)
		})
	}
}
