package darepod

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// identityKeyFamily is the key family used to derive the daemon's
// identity public key. This matches lnd's KeyFamilyNodeKey (family 6)
// so the derived key sits at m/1017'/coinType'/6'/0/0.
const identityKeyFamily = keychain.KeyFamilyNodeKey

// GenSeed generates a new aezeed cipher seed mnemonic. This is the
// first step when creating a new lwwallet-backed wallet. The returned
// mnemonic must be passed to InitWallet to finalize wallet creation.
// Only available in lwwallet mode when no wallet exists yet.
func (r *RPCServer) GenSeed(ctx context.Context,
	req *daemonrpc.GenSeedRequest) (*daemonrpc.GenSeedResponse,
	error) {

	// GenSeed is only available in lwwallet mode.
	if r.server.cfg.Wallet.Type != WalletTypeLwwallet {
		return nil, status.Errorf(codes.FailedPrecondition,
			"GenSeed is only available in lwwallet mode")
	}

	// GenSeed is only available when no wallet exists yet.
	if r.server.walletState != WalletStateNone {
		return nil, status.Errorf(codes.FailedPrecondition,
			"wallet already exists (state=%d)",
			r.server.walletState)
	}

	mnemonic, err := GenSeed(req.SeedPassphrase)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to generate seed: %v", err)
	}

	return &daemonrpc.GenSeedResponse{
		Mnemonic: mnemonic[:],
	}, nil
}

// InitWallet creates a new wallet from a previously generated aezeed
// mnemonic. The wallet is encrypted at rest with the provided
// password. Only available in lwwallet mode when no wallet exists.
func (r *RPCServer) InitWallet(ctx context.Context,
	req *daemonrpc.InitWalletRequest) (
	*daemonrpc.InitWalletResponse, error) {

	// InitWallet is only available in lwwallet mode.
	if r.server.cfg.Wallet.Type != WalletTypeLwwallet {
		return nil, status.Errorf(codes.FailedPrecondition,
			"InitWallet is only available in lwwallet mode")
	}

	// InitWallet is only available when no wallet exists yet.
	if r.server.walletState != WalletStateNone {
		return nil, status.Errorf(codes.FailedPrecondition,
			"wallet already exists (state=%d)",
			r.server.walletState)
	}

	// Validate the mnemonic length.
	if len(req.Mnemonic) != aezeed.NumMnemonicWords {
		return nil, status.Errorf(codes.InvalidArgument,
			"mnemonic must be %d words, got %d",
			aezeed.NumMnemonicWords, len(req.Mnemonic))
	}

	// Validate password length.
	if len(req.WalletPassword) < minPasswordLen {
		return nil, status.Errorf(codes.InvalidArgument,
			"wallet password must be at least %d bytes",
			minPasswordLen)
	}

	// Convert the string slice to an aezeed.Mnemonic array.
	var mnemonic aezeed.Mnemonic
	copy(mnemonic[:], req.Mnemonic)

	// Derive the raw seed from the mnemonic.
	seed, err := MnemonicToSeed(mnemonic, req.SeedPassphrase)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"invalid mnemonic: %v", err)
	}

	// Encrypt the seed at rest.
	ciphertext, err := EncryptSeed(seed, req.WalletPassword)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to encrypt seed: %v", err)
	}

	// Save the encrypted seed to disk.
	networkDir := r.server.cfg.NetworkDir()
	seedPath := SeedFilePath(networkDir)

	if err := SaveEncryptedSeed(seedPath, ciphertext); err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to save encrypted seed: %v", err)
	}

	log.InfoS(ctx, "Wallet seed encrypted and saved",
		"path", seedPath)

	// Start the lwwallet with the derived seed.
	if err := r.server.startLwwallet(ctx, seed); err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to start wallet: %v", err)
	}

	// Derive identity pubkey for the response.
	identityPubkey, err := r.deriveIdentityPubkey(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to derive identity pubkey: %v", err)
	}

	return &daemonrpc.InitWalletResponse{
		IdentityPubkey: identityPubkey,
	}, nil
}

// UnlockWallet decrypts an existing wallet seed using the provided
// password and starts the wallet subsystem. Only available in
// lwwallet mode when the wallet is locked.
func (r *RPCServer) UnlockWallet(ctx context.Context,
	req *daemonrpc.UnlockWalletRequest) (
	*daemonrpc.UnlockWalletResponse, error) {

	// UnlockWallet is only available in lwwallet mode.
	if r.server.cfg.Wallet.Type != WalletTypeLwwallet {
		return nil, status.Errorf(codes.FailedPrecondition,
			"UnlockWallet is only available in "+
				"lwwallet mode")
	}

	// UnlockWallet is only available when the wallet is locked.
	if r.server.walletState != WalletStateLocked {
		return nil, status.Errorf(codes.FailedPrecondition,
			"wallet is not locked (state=%d)",
			r.server.walletState)
	}

	// Validate password length.
	if len(req.WalletPassword) < minPasswordLen {
		return nil, status.Errorf(codes.InvalidArgument,
			"wallet password must be at least %d bytes",
			minPasswordLen)
	}

	// Load the encrypted seed from disk.
	networkDir := r.server.cfg.NetworkDir()
	seedPath := SeedFilePath(networkDir)

	ciphertext, err := LoadEncryptedSeed(seedPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to load encrypted seed: %v", err)
	}

	// Decrypt the seed.
	seed, err := DecryptSeed(ciphertext, req.WalletPassword)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"unable to decrypt seed: %v", err)
	}

	log.InfoS(ctx, "Wallet seed decrypted via UnlockWallet RPC")

	// Start the lwwallet with the decrypted seed.
	if err := r.server.startLwwallet(ctx, seed); err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to start wallet: %v", err)
	}

	// Derive identity pubkey for the response.
	identityPubkey, err := r.deriveIdentityPubkey(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to derive identity pubkey: %v", err)
	}

	return &daemonrpc.UnlockWalletResponse{
		IdentityPubkey: identityPubkey,
	}, nil
}

// deriveIdentityPubkey derives the node identity public key from the
// active lwwallet using KeyFamilyNodeKey (family 6, index 0). This
// matches lnd's identity key derivation path.
func (r *RPCServer) deriveIdentityPubkey(
	ctx context.Context) (string, error) {

	w := r.server.lwWallet.UnsafeFromSome()

	desc, err := w.DeriveNextKey(ctx, identityKeyFamily)
	if err != nil {
		return "", fmt.Errorf("derive identity key: %w", err)
	}

	return fmt.Sprintf("%x", desc.PubKey.SerializeCompressed()), nil
}
