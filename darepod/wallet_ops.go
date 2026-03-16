package darepod

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/aezeed"
)

// InitWalletFromMnemonic validates a mnemonic, derives the raw seed,
// encrypts it with the given password, and saves the ciphertext to
// disk. This is the core logic behind the InitWallet RPC, extracted
// as a package-level function so it can be reused by a future SDK
// that bypasses gRPC.
func InitWalletFromMnemonic(mnemonic []string, seedPassphrase,
	walletPassword []byte,
	networkDir string) ([rawSeedLen]byte, error) {

	// Validate the mnemonic length.
	if len(mnemonic) != aezeed.NumMnemonicWords {
		return [rawSeedLen]byte{}, fmt.Errorf(
			"mnemonic must be %d words, got %d",
			aezeed.NumMnemonicWords, len(mnemonic),
		)
	}

	// Validate password length.
	if len(walletPassword) < minPasswordLen {
		return [rawSeedLen]byte{}, fmt.Errorf(
			"wallet password must be at least %d bytes",
			minPasswordLen,
		)
	}

	// Convert the string slice to an aezeed.Mnemonic array.
	var m aezeed.Mnemonic
	copy(m[:], mnemonic)

	// Derive the raw seed from the mnemonic.
	seed, err := MnemonicToSeed(m, seedPassphrase)
	if err != nil {
		return [rawSeedLen]byte{}, fmt.Errorf(
			"invalid mnemonic: %w", err,
		)
	}

	// Encrypt the seed at rest.
	ciphertext, err := EncryptSeed(seed, walletPassword)
	if err != nil {
		return [rawSeedLen]byte{}, fmt.Errorf(
			"encrypting seed: %w", err,
		)
	}

	// Save the encrypted seed to disk.
	seedPath := SeedFilePath(networkDir)

	if err := SaveEncryptedSeed(seedPath, ciphertext); err != nil {
		return [rawSeedLen]byte{}, fmt.Errorf(
			"saving encrypted seed: %w", err,
		)
	}

	return seed, nil
}

// UnlockWalletFromDisk loads the encrypted seed from the network
// directory and decrypts it with the given password. This is the core
// logic behind the UnlockWallet RPC, extracted so it can be reused by
// a future SDK.
func UnlockWalletFromDisk(networkDir string,
	walletPassword []byte) ([rawSeedLen]byte, error) {

	// Validate password length.
	if len(walletPassword) < minPasswordLen {
		return [rawSeedLen]byte{}, fmt.Errorf(
			"wallet password must be at least %d bytes",
			minPasswordLen,
		)
	}

	// Load the encrypted seed from disk.
	seedPath := SeedFilePath(networkDir)

	ciphertext, err := LoadEncryptedSeed(seedPath)
	if err != nil {
		return [rawSeedLen]byte{}, fmt.Errorf(
			"loading encrypted seed: %w", err,
		)
	}

	// Decrypt the seed.
	seed, err := DecryptSeed(ciphertext, walletPassword)
	if err != nil {
		return [rawSeedLen]byte{}, fmt.Errorf(
			"decrypting seed: %w", err,
		)
	}

	return seed, nil
}

// BuildTransferInputs looks up full VTXO descriptors from the store
// for the given outpoints and converts them into OOR transfer inputs.
// This is extracted from the SendOOR RPC handler so a future SDK can
// prepare transfer inputs without going through gRPC.
func BuildTransferInputs(ctx context.Context,
	store vtxo.VTXOStore,
	outpoints []wire.OutPoint) ([]oor.TransferInput, error) {

	inputs := make([]oor.TransferInput, 0, len(outpoints))

	for _, op := range outpoints {
		desc, err := store.GetVTXO(ctx, op)
		if err != nil {
			return nil, fmt.Errorf(
				"look up VTXO %s: %w", op, err,
			)
		}

		// The checkpoint output collab path is a 2-of-2 multisig
		// between the VTXO owner and the operator, matching the
		// VTXO's own collaborative spend path. This ensures both
		// parties must sign the Ark tx that spends the checkpoint.
		collabLeaf, err := arkscript.MultiSigCollabTapLeaf(
			desc.ClientKey.PubKey, desc.OperatorKey,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"build collab leaf for %s: %w",
				op, err,
			)
		}

		inputs = append(inputs, oor.TransferInput{
			VTXO:            desc,
			OwnerLeafScript: collabLeaf.Script,
		})
	}

	return inputs, nil
}
