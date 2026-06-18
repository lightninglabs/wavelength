package darepod

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// identityKeyFamily is the key family used to derive the daemon wallet
// identity public key. This matches lnd's KeyFamilyNodeKey (family 6) so the
// derived key sits at m/1017'/coinType'/6'/0/0.
const identityKeyFamily = keychain.KeyFamilyNodeKey

const receiveAuthKeyTag = "darepo/swap/receive-auth/v1"

// ReceiveAuthKey returns the public key for the payment-scoped receive-auth
// key while keeping the private scalar inside the daemon.
func (r *RPCServer) ReceiveAuthKey(ctx context.Context,
	req *daemonrpc.ReceiveAuthKeyRequest) (
	*daemonrpc.ReceiveAuthKeyResponse, error) {

	privKey, err := r.receiveAuthPrivateKey(ctx, req.GetPaymentHash())
	if err != nil {
		return nil, err
	}

	return &daemonrpc.ReceiveAuthKeyResponse{
		Pubkey: privKey.PubKey().SerializeCompressed(),
	}, nil
}

// SignReceiveAuthMessage signs one message with the payment-scoped
// receive-auth key without returning private-key-equivalent material.
func (r *RPCServer) SignReceiveAuthMessage(ctx context.Context,
	req *daemonrpc.SignReceiveAuthMessageRequest) (
	*daemonrpc.SignReceiveAuthMessageResponse, error) {

	privKey, err := r.receiveAuthPrivateKey(ctx, req.GetPaymentHash())
	if err != nil {
		return nil, err
	}

	digest := receiveAuthDigest(req.GetMessage(), req.GetDoubleHash())
	sig := ecdsa.Sign(privKey, digest)

	return &daemonrpc.SignReceiveAuthMessageResponse{
		Signature: sig.Serialize(),
	}, nil
}

// SignReceiveAuthMessageCompact signs one message with the payment-scoped
// receive-auth key and returns a compact recoverable signature.
func (r *RPCServer) SignReceiveAuthMessageCompact(ctx context.Context,
	req *daemonrpc.SignReceiveAuthMessageCompactRequest) (
	*daemonrpc.SignReceiveAuthMessageCompactResponse, error) {

	privKey, err := r.receiveAuthPrivateKey(ctx, req.GetPaymentHash())
	if err != nil {
		return nil, err
	}

	digest := receiveAuthDigest(req.GetMessage(), req.GetDoubleHash())
	sig := ecdsa.SignCompact(privKey, digest, true)

	return &daemonrpc.SignReceiveAuthMessageCompactResponse{
		Signature: sig,
	}, nil
}

// ReceiveAuthECDH derives one Sphinx shared secret with the payment-scoped
// receive-auth key without exposing the receive-auth scalar to the caller.
func (r *RPCServer) ReceiveAuthECDH(ctx context.Context,
	req *daemonrpc.ReceiveAuthECDHRequest) (
	*daemonrpc.ReceiveAuthECDHResponse, error) {

	privKey, err := r.receiveAuthPrivateKey(ctx, req.GetPaymentHash())
	if err != nil {
		return nil, err
	}

	pubKey, err := btcec.ParsePubKey(req.GetPubkey())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"pubkey: %v", err)
	}

	sharedSecret := receiveAuthECDH(privKey, pubKey)

	return &daemonrpc.ReceiveAuthECDHResponse{
		SharedSecret: sharedSecret[:],
	}, nil
}

// deriveReceiveAuthKey derives the receive-auth base using the same
// Diffie-Hellman construction as lnd's Signer.DeriveSharedKey RPC, then
// domain-separates it for one payment hash.
func (r *RPCServer) deriveReceiveAuthKey(ctx context.Context,
	paymentHash lntypes.Hash) ([32]byte, error) {

	keyDesc := keychain.KeyDescriptor{
		KeyLocator: *lndclient.SharedKeyLocator,
	}
	var (
		base [32]byte
		err  error
	)

	switch r.server.cfg.Wallet.Type {
	case WalletTypeLnd:
		if !r.server.lnd.IsSome() {
			return [32]byte{}, fmt.Errorf("lnd wallet not " +
				"connected")
		}

		lndSvc := r.server.lnd.UnsafeFromSome()

		base, err = lndSvc.Signer.DeriveSharedKey(
			ctx, lndclient.SharedKeyNUMS,
			lndclient.SharedKeyLocator,
		)

	case WalletTypeLwwallet:
		if !r.server.lwWallet.IsSome() {
			return [32]byte{}, fmt.Errorf("lwwallet not " +
				"initialized")
		}

		w := r.server.lwWallet.UnsafeFromSome()

		base, err = w.KeyRing().ECDH(keyDesc, lndclient.SharedKeyNUMS)

	case WalletTypeBtcwallet:
		if !r.server.btcwWallet.IsSome() {
			return [32]byte{}, fmt.Errorf("btcwallet not " +
				"initialized")
		}

		w := r.server.btcwWallet.UnsafeFromSome()

		base, err = w.KeyRing().ECDH(
			keyDesc, lndclient.SharedKeyNUMS,
		)

	default:
		return [32]byte{}, fmt.Errorf("receive auth key derivation "+
			"not supported for wallet type %q",
			r.server.cfg.Wallet.Type)
	}
	if err != nil {
		return [32]byte{}, err
	}

	key := chainhash.TaggedHash(
		[]byte(receiveAuthKeyTag), base[:], paymentHash[:],
	)

	return [32]byte(*key), nil
}

// receiveAuthPrivateKey validates the request payment hash and derives the
// matching private key inside the daemon process.
func (r *RPCServer) receiveAuthPrivateKey(ctx context.Context,
	rawPaymentHash []byte) (*btcec.PrivateKey, error) {

	if len(rawPaymentHash) != lntypes.HashSize {
		return nil, status.Errorf(codes.InvalidArgument,
			"payment_hash must be %d bytes", lntypes.HashSize)
	}
	var paymentHash lntypes.Hash
	copy(paymentHash[:], rawPaymentHash)

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	key, err := r.deriveReceiveAuthKey(ctx, paymentHash)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "derive receive "+
			"auth key: %v", err)
	}

	privKey, _ := btcec.PrivKeyFromBytes(key[:])

	return privKey, nil
}

// receiveAuthDigest matches keychain.PrivKeyMessageSigner's hashing behavior
// so daemon-backed receive-auth signatures are byte-for-byte compatible.
func receiveAuthDigest(message []byte, doubleHash bool) []byte {
	if doubleHash {
		return chainhash.DoubleHashB(message)
	}

	return chainhash.HashB(message)
}

// receiveAuthECDH matches the lightning-onion SingleKeyECDH contract by
// returning the SHA256 of the compressed shared point.
func receiveAuthECDH(privKey *btcec.PrivateKey,
	pubKey *btcec.PublicKey) [32]byte {

	var pubJ btcec.JacobianPoint
	pubKey.AsJacobian(&pubJ)

	var ecdhPoint btcec.JacobianPoint
	btcec.ScalarMultNonConst(&privKey.Key, &pubJ, &ecdhPoint)

	ecdhPoint.ToAffine()
	ecdhPubKey := btcec.NewPublicKey(&ecdhPoint.X, &ecdhPoint.Y)

	return sha256.Sum256(ecdhPubKey.SerializeCompressed())
}

// GenSeed generates a new aezeed cipher seed mnemonic. This is the
// first step when creating a new lwwallet-backed wallet. The returned
// mnemonic must be passed to InitWallet to finalize wallet creation.
// Only available in lwwallet mode when no wallet exists yet.
func (r *RPCServer) GenSeed(ctx context.Context,
	req *daemonrpc.GenSeedRequest) (*daemonrpc.GenSeedResponse, error) {

	// GenSeed is only available in lwwallet/btcwallet mode.
	if !r.server.isSelfManagedWallet() {
		return nil, status.Errorf(codes.FailedPrecondition, "GenSeed "+
			"is only available in lwwallet/btcwallet mode")
	}

	// GenSeed is only available when no wallet exists yet.
	currentState := WalletState(r.server.walletState.Load())
	if currentState != WalletStateNone {
		return nil, status.Errorf(codes.FailedPrecondition, "wallet "+
			"already exists (state=%s)", currentState)
	}

	mnemonic, err := GenerateSeed(req.SeedPassphrase)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to generate "+
			"seed: %v", err)
	}

	return &daemonrpc.GenSeedResponse{
		Mnemonic: mnemonic[:],
	}, nil
}

// InitWallet creates a new wallet from a previously generated aezeed
// mnemonic. The wallet is encrypted at rest with the provided
// password. Only available in lwwallet mode when no wallet exists.
func (r *RPCServer) InitWallet(ctx context.Context,
	req *daemonrpc.InitWalletRequest) (*daemonrpc.InitWalletResponse,
	error) {

	// InitWallet is only available in lwwallet/btcwallet mode.
	if !r.server.isSelfManagedWallet() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"InitWallet is only available in lwwallet/btcwallet "+
				"mode")
	}

	// Atomically check that no wallet exists and claim the
	// transition so a concurrent InitWallet call cannot race
	// past this point. CompareAndSwap ensures only one caller
	// wins the None → Locked transition.
	if !r.server.walletState.CompareAndSwap(
		int32(WalletStateNone), int32(WalletStateLocked),
	) {

		state := WalletState(r.server.walletState.Load())

		return nil, status.Errorf(codes.FailedPrecondition, "wallet "+
			"already exists (state=%s)", state)
	}

	// rollbackState resets the wallet state to None so that a
	// subsequent InitWallet call can retry after a transient
	// failure (bad mnemonic, disk error, etc.).
	rollbackState := func() {
		r.server.walletState.Store(int32(WalletStateNone))
	}

	// Resolve the network directory for seed storage.
	networkDir := r.server.cfg.NetworkDir()

	// Delegate to the package-level function that validates the
	// mnemonic, derives the seed, encrypts it, and saves it to
	// disk. This logic is extracted so a future SDK can call it
	// directly without going through gRPC.
	seed, birthday, err := InitWalletFromMnemonicWithBirthday(
		req.Mnemonic, req.SeedPassphrase, req.WalletPassword,
		networkDir,
	)
	if err != nil {
		rollbackState()

		return nil, status.Errorf(codes.Internal, "unable to "+
			"initialize wallet: %v", err)
	}

	r.server.log.InfoS(ctx, "Wallet seed encrypted and saved",
		"path", SeedFilePath(networkDir),
	)

	// Start the wallet with the derived seed.
	if err := r.server.startSelfManagedWallet(
		ctx, seed, birthday,
	); err != nil {

		rollbackState()

		return nil, status.Errorf(codes.Internal, "unable to start "+
			"wallet: %v", err)
	}

	var recoveryResult *walletRecoveryResult
	if req.GetRecoverState() {
		recoveryResult, err = r.recoverWalletState(
			ctx, req.GetRecoveryWindow(),
		)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "unable to "+
				"recover wallet state: %v", err)
		}
	}

	// Derive identity pubkey for the response.
	identityPubkey, err := r.deriveIdentityPubkey(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to derive "+
			"identity pubkey: %v", err)
	}

	resp := &daemonrpc.InitWalletResponse{
		IdentityPubkey: identityPubkey,
	}
	if recoveryResult != nil {
		recoveryResult.apply(resp)
	}

	return resp, nil
}

// UnlockWallet decrypts an existing wallet seed using the provided
// password and starts the wallet subsystem. Only available in
// lwwallet mode when the wallet is locked.
func (r *RPCServer) UnlockWallet(ctx context.Context,
	req *daemonrpc.UnlockWalletRequest) (*daemonrpc.UnlockWalletResponse,
	error) {

	// UnlockWallet is only available in lwwallet/btcwallet mode.
	if !r.server.isSelfManagedWallet() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"UnlockWallet is only available in "+
				"lwwallet/btcwallet mode")
	}

	// Atomically claim the Locked -> Unlocking transition before
	// decrypting or starting the wallet backend so concurrent unlock
	// attempts cannot start multiple wallet instances.
	if !r.server.walletState.CompareAndSwap(
		int32(WalletStateLocked), int32(WalletStateUnlocking),
	) {

		currentState := WalletState(r.server.walletState.Load())

		return nil, status.Errorf(codes.FailedPrecondition, "wallet "+
			"is not locked (state=%s)", currentState)
	}

	rollbackState := func() {
		r.server.walletState.Store(int32(WalletStateLocked))
	}

	// Resolve the network directory for seed lookup.
	networkDir := r.server.cfg.NetworkDir()

	// Delegate to the package-level function that loads the
	// encrypted seed from disk and decrypts it. This logic is
	// extracted so a future SDK can call it directly.
	seed, err := UnlockWalletFromDisk(
		networkDir, req.WalletPassword,
	)
	if err != nil {
		rollbackState()

		return nil, status.Errorf(codes.Internal, "unable to unlock "+
			"wallet: %v", err)
	}

	r.server.log.InfoS(ctx, "Wallet seed decrypted via UnlockWallet RPC")

	// Start the wallet with the decrypted seed.
	if err := r.server.startSelfManagedWallet(
		ctx, seed, time.Time{},
	); err != nil {

		rollbackState()

		return nil, status.Errorf(codes.Internal, "unable to start "+
			"wallet: %v", err)
	}

	// Derive identity pubkey for the response.
	identityPubkey, err := r.deriveIdentityPubkey(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to derive "+
			"identity pubkey: %v", err)
	}

	return &daemonrpc.UnlockWalletResponse{
		IdentityPubkey: identityPubkey,
	}, nil
}

// deriveIdentityPubkey derives the daemon wallet identity public key from the
// active self-managed wallet using KeyFamilyNodeKey (family 6, index 0). This
// is the key used for Ark/OOR signing. DeriveKey (not DeriveNextKey) is used so
// the identity key is stable across calls.
func (r *RPCServer) deriveIdentityPubkey(ctx context.Context) (string, error) {
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
		// GetInfo is intentionally callable before InitWallet /
		// UnlockWallet (clients probe WalletReady via GetInfo).
		// Guard the Option so we surface a structured error
		// instead of panicking on UnsafeFromSome for a wallet
		// that has not been initialized yet.
		if !r.server.lwWallet.IsSome() {
			return "", fmt.Errorf("lwwallet not initialized")
		}

		w := r.server.lwWallet.UnsafeFromSome()
		desc, err = w.DeriveKey(ctx, loc)

	case WalletTypeBtcwallet:
		if !r.server.btcwWallet.IsSome() {
			return "", fmt.Errorf("btcwallet not initialized")
		}

		w := r.server.btcwWallet.UnsafeFromSome()
		desc, err = w.DeriveKey(ctx, loc)

	default:
		return "", fmt.Errorf("deriveIdentityPubkey not supported for "+
			"wallet type %q", r.server.cfg.Wallet.Type)
	}
	if err != nil {
		return "", fmt.Errorf("derive identity key: %w", err)
	}

	return fmt.Sprintf("%x", desc.PubKey.SerializeCompressed()), nil
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
	seed [rawSeedLen]byte, birthday time.Time) error {

	switch s.cfg.Wallet.Type {
	case WalletTypeLwwallet:
		return s.startLwwallet(ctx, seed, birthday)

	case WalletTypeBtcwallet:
		return s.startBtcwallet(ctx, seed, birthday)

	default:
		return fmt.Errorf("unsupported wallet type %q",
			s.cfg.Wallet.Type)
	}
}
