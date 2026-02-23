package round

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// joinRoundAuthIdentifierKeyFamily is the BIP32 key family used
	// for per-request join authorization identifier keys.
	joinRoundAuthIdentifierKeyFamily keychain.KeyFamily = 43
)

// deriveJoinAuthIdentifierKey derives a fresh key descriptor used as the
// join-request BIP-322 identifier and challenge key.
func deriveJoinAuthIdentifierKey(ctx context.Context,
	wallet ClientWallet) (keychain.KeyDescriptor, error) {

	if wallet == nil {
		return keychain.KeyDescriptor{}, fmt.Errorf(
			"wallet signer must be provided",
		)
	}

	keyDesc, err := wallet.DeriveNextKey(
		ctx, joinRoundAuthIdentifierKeyFamily,
	)
	if err != nil {
		return keychain.KeyDescriptor{}, fmt.Errorf(
			"derive join auth identifier key: %w", err,
		)
	}

	if keyDesc == nil {
		return keychain.KeyDescriptor{}, fmt.Errorf(
			"derive join auth identifier key returned nil " +
				"descriptor",
		)
	}

	if keyDesc.PubKey == nil {
		return keychain.KeyDescriptor{}, fmt.Errorf(
			"derive join auth identifier key returned nil " +
				"pubkey",
		)
	}

	return *keyDesc, nil
}
