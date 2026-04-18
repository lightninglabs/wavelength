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

	// GenSeed is only available in lwwallet/btcwallet mode.
	if !r.server.isSelfManagedWallet() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"GenSeed is only available in lwwallet/"+
				"btcwallet mode")
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

	// InitWallet is only available in lwwallet/btcwallet mode.
	if !r.server.isSelfManagedWallet() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"InitWallet is only available in lwwallet/"+
				"btcwallet mode")
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

	r.server.log.InfoS(ctx, "Wallet seed encrypted and saved",
		"path", SeedFilePath(networkDir))

	// Start the wallet with the derived seed.
	if err := r.server.startSelfManagedWallet(ctx, seed); err != nil {
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

	// UnlockWallet is only available in lwwallet/btcwallet mode.
	if !r.server.isSelfManagedWallet() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"UnlockWallet is only available in "+
				"lwwallet/btcwallet mode")
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

	r.server.log.InfoS(ctx, "Wallet seed decrypted via UnlockWallet RPC")

	// Start the wallet with the decrypted seed.
	if err := r.server.startSelfManagedWallet(ctx, seed); err != nil {
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
// active self-managed wallet using KeyFamilyNodeKey (family 6, index
// 0). This matches lnd's identity key derivation path. DeriveKey (not
// DeriveNextKey) is used so the identity key is stable across calls.
func (r *RPCServer) deriveIdentityPubkey(
	ctx context.Context) (string, error) {

	loc := keychain.KeyLocator{
		Family: identityKeyFamily,
		Index:  0,
	}

	var (
		desc *keychain.KeyDescriptor
		err  error
	)

	switch r.server.cfg.Wallet.Type {
	case WalletTypeLnd:
		if !r.server.lnd.IsSome() {
			return "", fmt.Errorf("lnd wallet not connected")
		}

		lndSvc := r.server.lnd.UnsafeFromSome()
		desc, err = lndSvc.WalletKit.DeriveKey(ctx, &loc)

	case WalletTypeLwwallet:
		w := r.server.lwWallet.UnsafeFromSome()
		desc, err = w.DeriveKey(ctx, loc)

	case WalletTypeBtcwallet:
		w := r.server.btcwWallet.UnsafeFromSome()
		desc, err = w.DeriveKey(ctx, loc)

	default:
		return "", fmt.Errorf("deriveIdentityPubkey not "+
			"supported for wallet type %q",
			r.server.cfg.Wallet.Type)
	}
	if err != nil {
		return "", fmt.Errorf("derive identity key: %w", err)
	}

	return fmt.Sprintf(
		"%x", desc.PubKey.SerializeCompressed(),
	), nil
}

// isSelfManagedWallet returns true if the wallet type manages its
// own seed (lwwallet or btcwallet), as opposed to LND which manages
// the wallet externally.
func (s *Server) isSelfManagedWallet() bool {
	return s.cfg.Wallet.Type == WalletTypeLwwallet ||
		s.cfg.Wallet.Type == WalletTypeBtcwallet
}

// startSelfManagedWallet starts the appropriate self-managed wallet
// based on the configured wallet type.
func (s *Server) startSelfManagedWallet(ctx context.Context,
	seed [rawSeedLen]byte) error {

	switch s.cfg.Wallet.Type {
	case WalletTypeLwwallet:
		return s.startLwwallet(ctx, seed)

	case WalletTypeBtcwallet:
		return s.startBtcwallet(ctx, seed)

	default:
		return fmt.Errorf("unsupported wallet type %q",
			s.cfg.Wallet.Type)
	}
}
