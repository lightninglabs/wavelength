package darepod

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightninglabs/darepo-client/daemonrpc"
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

// BuildCustomTransferInputs constructs OOR transfer inputs from
// explicit custom input specifications. This bypasses wallet VTXO
// selection for non-standard spend paths (e.g., vHTLC claims).
func BuildCustomTransferInputs(ctx context.Context,
	store vtxo.VTXOStore,
	customInputs []*daemonrpc.CustomOORInput,
	clientKey keychain.KeyDescriptor,
	operatorKey *btcec.PublicKey,
	exitDelay uint32) (
	[]oor.TransferInput, error) {

	inputs := make([]oor.TransferInput, 0, len(customInputs))

	for _, ci := range customInputs {
		outpoint, err := parseOutpointString(ci.Outpoint)
		if err != nil {
			return nil, fmt.Errorf("parse outpoint %q: %w",
				ci.Outpoint, err)
		}

		var desc *vtxo.Descriptor

		// If the caller provided amount and pkscript, build
		// the descriptor directly (for VTXOs not in the local
		// store, e.g., received via OOR).
		if ci.AmountSat > 0 && len(ci.PkScript) > 0 {
			desc = &vtxo.Descriptor{
				Outpoint:       outpoint,
				Amount:         btcutil.Amount(ci.AmountSat),
				PkScript:       ci.PkScript,
				ClientKey:      clientKey,
				OperatorKey:    operatorKey,
				RelativeExpiry: exitDelay,
			}
		} else {
			// Fall back to store lookup.
			desc, err = store.GetVTXO(ctx, outpoint)
			if err != nil {
				return nil, fmt.Errorf(
					"look up VTXO %s: %w",
					outpoint, err)
			}
		}

		input := oor.TransferInput{
			VTXO: desc,
		}

		// Attach custom spend path if provided.
		if len(ci.SpendWitnessScript) > 0 {
			input.CustomSpend = &arkscript.SpendPath{
				SpendInfo: &arkscript.SpendInfo{
					WitnessScript: ci.SpendWitnessScript,
					ControlBlock:  ci.SpendControlBlock,
				},
				Conditions: ci.ConditionWitness,
			}
		}

		inputs = append(inputs, input)
	}

	return inputs, nil
}

