package darepod

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/daemonrpc"
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
	currentState := WalletState(r.server.walletState.Load())
	if currentState != WalletStateNone {
		return nil, status.Errorf(codes.FailedPrecondition,
			"wallet already exists (state=%d)",
			currentState)
	}

	mnemonic, err := GenerateSeed(req.SeedPassphrase)
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

	// Atomically check that no wallet exists and claim the
	// transition so a concurrent InitWallet call cannot race
	// past this point. CompareAndSwap ensures only one caller
	// wins the None → Locked transition.
	if !r.server.walletState.CompareAndSwap(
		int32(WalletStateNone), int32(WalletStateLocked),
	) {

		state := WalletState(r.server.walletState.Load())

		return nil, status.Errorf(codes.FailedPrecondition,
			"wallet already exists (state=%d)", state)
	}

	// rollbackState resets the wallet state to None so that a
	// subsequent InitWallet call can retry after a transient
	// failure (bad mnemonic, disk error, etc.).
	rollbackState := func() {
		r.server.walletState.Store(int32(WalletStateNone))
	}

	// Resolve the network directory for seed storage.
	networkDir, err := r.server.cfg.NetworkDir()
	if err != nil {
		rollbackState()

		return nil, status.Errorf(codes.Internal,
			"unable to resolve network directory: %v", err)
	}

	// Delegate to the package-level function that validates the
	// mnemonic, derives the seed, encrypts it, and saves it to
	// disk. This logic is extracted so a future SDK can call it
	// directly without going through gRPC.
	seed, err := InitWalletFromMnemonic(
		req.Mnemonic, req.SeedPassphrase,
		req.WalletPassword, networkDir,
	)
	if err != nil {
		rollbackState()

		return nil, status.Errorf(codes.Internal,
			"unable to initialize wallet: %v", err)
	}

	log.InfoS(ctx, "Wallet seed encrypted and saved",
		"path", SeedFilePath(networkDir))

	// Start the lwwallet with the derived seed.
	if err := r.server.startLwwallet(ctx, seed); err != nil {
		rollbackState()

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

	// Atomically verify the wallet is locked. We do not need a CAS
	// here because startLwwallet (called below) will perform the
	// Locked → Ready transition via markWalletReady. A concurrent
	// UnlockWallet that passes this check will fail inside
	// startLwwallet when it tries to start a second wallet.
	currentState := WalletState(r.server.walletState.Load())
	if currentState != WalletStateLocked {
		return nil, status.Errorf(codes.FailedPrecondition,
			"wallet is not locked (state=%d)",
			currentState)
	}

	// Resolve the network directory for seed lookup.
	networkDir, err := r.server.cfg.NetworkDir()
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to resolve network directory: %v", err)
	}

	// Delegate to the package-level function that loads the
	// encrypted seed from disk and decrypts it. This logic is
	// extracted so a future SDK can call it directly.
	seed, err := UnlockWalletFromDisk(
		networkDir, req.WalletPassword,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to unlock wallet: %v", err)
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
// matches lnd's identity key derivation path. DeriveKey (not
// DeriveNextKey) is used so the identity key is stable across calls.
func (r *RPCServer) deriveIdentityPubkey(
	ctx context.Context) (string, error) {

	w := r.server.lwWallet.UnsafeFromSome()

	desc, err := w.DeriveKey(ctx, keychain.KeyLocator{
		Family: identityKeyFamily,
		Index:  0,
	})
	if err != nil {
		return "", fmt.Errorf("derive identity key: %w", err)
	}

	return fmt.Sprintf("%x", desc.PubKey.SerializeCompressed()), nil
}
