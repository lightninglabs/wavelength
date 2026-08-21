package wavewalletdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightningnetwork/lnd/aezeed"
	"golang.org/x/crypto/hkdf"
)

const (
	externalSeedDBKeyInfo = "wavewalletdk:external-seed:dbpw:v1"

	wavedSeedEnvVar     = "WAVED_LWWALLET_SEED"
	wavedPasswordEnvVar = "WAVED_WALLET_PASSWORD" //nolint:gosec
)

// ExternalSeedWalletRequest opens the wallet at an explicit daemon data
// directory from already-derived aezeed entropy. SeedEntropy must contain
// exactly aezeed.EntropySize bytes. Wavelength does not assign BIP39, BIP32,
// account-index, network, or storage-path semantics to those bytes; the host
// SDK owns that versioned derivation and selects Config.DataDir.
//
// ExpectedIdentityPubKey optionally binds the selected directory to a
// previously persisted Wavelength identity. RecoverState is an explicit
// per-open operation: setting it on an existing wallet reruns the idempotent
// recovery scan, including after a prior scan failed. Callers should leave it
// false for ordinary starts after recovery has succeeded.
type ExternalSeedWalletRequest struct {
	SeedEntropy            []byte
	ExpectedIdentityPubKey string
	RecoverState           bool
	RecoveryWindow         uint32
}

// StartExternalSeedWallet atomically starts an embedded daemon at the explicit
// Config.DataDir, then imports a fresh wallet or unlocks its existing wallet
// from externally derived entropy. If opening fails, the newly started daemon
// is stopped before the error returns.
func StartExternalSeedWallet(ctx context.Context, cfg Config,
	req ExternalSeedWalletRequest, opts ...Option) (*Client,
	*ExternalSeedWalletOpenResult, error) {

	return StartExternalSeedWalletWithContexts(
		ctx, ctx, cfg, req, opts...,
	)
}

// StartExternalSeedWalletWithContexts starts an embedded daemon with startCtx,
// then imports or unlocks its wallet with openCtx. It returns only after
// wallet-dependent actors and mailbox ingress are ready, so the first wallet
// operation cannot race their initialization. Authenticated terms refreshes
// and wallet-ready hooks are outside this success boundary. The split lets
// hosts keep a short daemon-boot deadline without imposing that deadline on
// wallet opening, service initialization, or an optional state-recovery scan.
// If opening or pre-readiness service startup fails, the newly started daemon
// is stopped before the error returns.
func StartExternalSeedWalletWithContexts(startCtx, openCtx context.Context,
	cfg Config, req ExternalSeedWalletRequest, opts ...Option) (*Client,
	*ExternalSeedWalletOpenResult, error) {

	seedEntropy := bytes.Clone(req.SeedEntropy)
	defer clear(seedEntropy)

	if err := validateExternalSeedWalletRequest(seedEntropy); err != nil {
		return nil, nil, err
	}
	if err := validateExternalSeedWalletConfig(cfg); err != nil {
		return nil, nil, err
	}

	// The seed and password environment hooks are process-global and take
	// precedence during daemon startup. Reject them before Start can create
	// an unrelated wallet or auto-unlock one without proving this request's
	// derived password.
	for _, envVar := range []string{wavedSeedEnvVar, wavedPasswordEnvVar} {
		if value, ok := os.LookupEnv(envVar); ok && value != "" {
			return nil, nil, fmt.Errorf("%s must be unset when "+
				"starting an external-seed wallet", envVar)
		}
	}
	if err := verifyExpectedExternalSeedWalletIdentity(
		cfg, seedEntropy, req.ExpectedIdentityPubKey,
	); err != nil {
		return nil, nil, err
	}

	client, err := Start(startCtx, cfg, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("start external-seed wallet: %w",
			err)
	}

	result, err := openStartedExternalSeedWallet(
		openCtx, client, req, seedEntropy,
	)
	if err != nil {
		return nil, nil, err
	}

	return client, result, nil
}

// openStartedExternalSeedWallet opens the wallet on an already-started
// embedded client and rolls the daemon back on either open or readiness
// failure. Keeping both post-Start failure paths here lets tests pin the
// atomic stop-and-join contract without replacing the production starter.
func openStartedExternalSeedWallet(ctx context.Context, client *Client,
	req ExternalSeedWalletRequest,
	seedEntropy []byte) (*ExternalSeedWalletOpenResult, error) {

	openReq := req
	openReq.SeedEntropy = seedEntropy
	result, err := client.openWalletFromExternalSeed(ctx, openReq)
	if err != nil {
		// Stop owns its own bounded daemon-shutdown context.
		//nolint:contextcheck
		stopErr := client.Stop()

		return nil, fmt.Errorf("open external-seed wallet: %w",
			errors.Join(err, stopErr))
	}

	if err := waitForExternalSeedWalletReady(ctx, client); err != nil {
		// Stop owns its own bounded daemon-shutdown context.
		//nolint:contextcheck
		stopErr := client.Stop()

		return nil, fmt.Errorf("wait for external-seed wallet "+
			"ready: %w", errors.Join(err, stopErr))
	}

	return result, nil
}

// waitForExternalSeedWalletReady waits for the embedded daemon's
// wallet-services signal, which succeeds after wallet-dependent actors and
// mailbox ingress have started. WalletStateReady alone is too early for calls
// such as Deposit because the wallet backend publishes that state before those
// services finish initializing.
func waitForExternalSeedWalletReady(ctx context.Context, client *Client) error {
	if client == nil || client.waitWalletServicesReady == nil {
		return errors.New("external-seed wallet readiness requires " +
			"an embedded client")
	}

	return client.waitWalletServicesReady(ctx)
}

// openWalletFromExternalSeed converts exactly 16 externally derived bytes to
// fixed-birthday aezeed entropy and either imports it into an empty daemon or
// unlocks the existing local wallet. RecoverState is honored on both first
// import and existing-wallet unlock. It remains private so remote clients
// cannot inject seed material through a generic wallet RPC.
func (c *Client) openWalletFromExternalSeed(ctx context.Context,
	req ExternalSeedWalletRequest) (*ExternalSeedWalletOpenResult, error) {

	if err := validateExternalSeedWalletRequest(
		req.SeedEntropy,
	); err != nil {
		return nil, err
	}

	var entropy [aezeed.EntropySize]byte
	copy(entropy[:], req.SeedEntropy)
	defer clear(entropy[:])

	dbPassword := deriveExternalSeedDBPassword(entropy)
	defer clear(dbPassword)

	info, err := c.GetInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("read wallet state: %w", err)
	}

	switch info.WalletState {
	case WalletStateNone:
		return c.createWalletFromExternalSeed(
			ctx, entropy, dbPassword, req,
		)

	case WalletStateLocked:
		if req.RecoverState && c.recoverWalletState == nil {
			return nil, errors.New("wallet state recovery " +
				"requires an embedded client")
		}

		unlock, err := c.UnlockWallet(ctx, UnlockWalletRequest{
			WalletPassword: dbPassword,
		})
		if err != nil {
			return nil, err
		}
		if err := verifyExternalSeedWalletIdentity(
			req.ExpectedIdentityPubKey, unlock.IdentityPubKey,
		); err != nil {
			return nil, err
		}

		result := &ExternalSeedWalletOpenResult{
			Imported:       false,
			IdentityPubKey: unlock.IdentityPubKey,
		}
		if !req.RecoverState {
			return result, nil
		}
		if err := c.recoverExternalSeedWalletState(
			ctx, req.RecoveryWindow, result,
		); err != nil {
			return nil, err
		}

		return result, nil

	case WalletStateReady, WalletStateSyncing:
		// Without an unlock attempt, the daemon cannot prove that its
		// open wallet came from the supplied entropy. Match the
		// existing passkey behavior and refuse to claim that
		// association.
		return nil, errors.New("wallet is already unlocked")

	default:
		return nil, fmt.Errorf("unexpected wallet state %v: cannot "+
			"open wallet", info.WalletState)
	}
}

// createWalletFromExternalSeed converts fixed entropy into the daemon's
// existing aezeed import format and initializes the empty local wallet.
func (c *Client) createWalletFromExternalSeed(ctx context.Context,
	entropy [aezeed.EntropySize]byte, dbPassword []byte,
	req ExternalSeedWalletRequest) (*ExternalSeedWalletOpenResult, error) {

	defer clear(entropy[:])
	if req.RecoverState && c.recoverWalletState == nil {
		return nil, errors.New("wallet state recovery requires an " +
			"embedded client")
	}

	mnemonic, err := entropyToMnemonic(entropy)
	if err != nil {
		return nil, fmt.Errorf("derive aezeed mnemonic: %w", err)
	}
	defer clear(mnemonic[:])

	created, err := c.CreateWallet(ctx, CreateWalletRequest{
		Mnemonic:       mnemonic[:],
		WalletPassword: dbPassword,
		// Identity must be verified before recovery can register or
		// persist state for this new profile. Run the same explicit,
		// idempotent recovery operation used by locked-wallet retries
		// only after the check below succeeds.
		RecoverState: false,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize wallet from external "+
			"entropy: %w", err)
	}
	defer clear(created.Mnemonic)
	defer clear(created.EncipheredSeed)

	if err := verifyExternalSeedWalletIdentity(
		req.ExpectedIdentityPubKey, created.IdentityPubKey,
	); err != nil {
		return nil, err
	}

	result := &ExternalSeedWalletOpenResult{
		Imported:       true,
		IdentityPubKey: created.IdentityPubKey,
	}
	if !req.RecoverState {
		return result, nil
	}
	if err := c.recoverExternalSeedWalletState(
		ctx, req.RecoveryWindow, result,
	); err != nil {
		return nil, err
	}

	return result, nil
}

// recoverExternalSeedWalletState runs the private embedded recovery operation
// after the selected profile's identity has been verified and copies its
// counters into the public open result.
func (c *Client) recoverExternalSeedWalletState(ctx context.Context,
	recoveryWindow uint32, result *ExternalSeedWalletOpenResult) error {

	if c.recoverWalletState == nil {
		return errors.New("wallet state recovery requires an " +
			"embedded client")
	}

	recovery, err := c.recoverWalletState(ctx, recoveryWindow)
	if err != nil {
		return fmt.Errorf("recover wallet state: %w", err)
	}
	if recovery == nil {
		return errors.New("recover wallet state: empty result")
	}

	result.RecoveryRan = true
	result.RecoveredBoardingAddresses = recovery.BoardingAddresses
	result.RecoveredBoardingUTXOs = recovery.BoardingUTXOs
	result.RecoveredVTXOs = recovery.VTXOs
	result.RecoveredOORReceiveScripts = recovery.OORReceiveScripts
	result.RecoveredOORRecipientEvents = recovery.OOREvents

	return nil
}

// deriveExternalSeedDBPassword derives a domain-separated password for the
// local encrypted wallet DB. It is deliberately not the seed entropy itself.
func deriveExternalSeedDBPassword(entropy [aezeed.EntropySize]byte) []byte {
	defer clear(entropy[:])

	rawPassword := make([]byte, 32)
	reader := hkdf.New(
		sha256.New, entropy[:], nil, []byte(externalSeedDBKeyInfo),
	)
	_, _ = io.ReadFull(reader, rawPassword)
	defer clear(rawPassword)

	password := make([]byte, hex.EncodedLen(len(rawPassword)))
	hex.Encode(password, rawPassword)

	return password
}

// verifyExpectedExternalSeedWalletIdentity proves an optional identity pin
// directly from the requested entropy before Start can create a directory or
// initialize a wallet database. The post-open check remains as a defense
// against future drift between this derivation and the wallet backend.
func verifyExpectedExternalSeedWalletIdentity(cfg Config, entropy []byte,
	expected string) error {

	if expected == "" {
		return nil
	}

	daemonCfg, err := daemonConfig(cfg)
	if err != nil {
		return fmt.Errorf("resolve external-seed wallet network: %w",
			err)
	}

	var rawSeed [32]byte
	copy(rawSeed[:], entropy)
	defer clear(rawSeed[:])

	actual, err := waved.WalletIdentityPubKeyFromSeed(
		rawSeed, daemonCfg.Network,
	)
	if err != nil {
		return fmt.Errorf("derive external-seed wallet identity: %w",
			err)
	}

	return verifyExternalSeedWalletIdentity(expected, actual)
}

// validateExternalSeedWalletRequest requires the complete aezeed entropy
// boundary. Accepting any other size would require an undocumented transform
// and make host implementations derive different wallets from the same input.
func validateExternalSeedWalletRequest(seedEntropy []byte) error {
	if len(seedEntropy) != aezeed.EntropySize {
		return fmt.Errorf("external seed entropy must be exactly %d "+
			"bytes: got %d", aezeed.EntropySize, len(seedEntropy))
	}

	return nil
}

// validateExternalSeedWalletConfig checks the storage and wallet-backend
// boundary before Start performs any filesystem or network I/O. The host owns
// path derivation, so it must supply the final DataDir explicitly. Alternate
// state and auto-unlock paths could escape that selected directory or bypass
// proof that the existing DB belongs to this request, so they are rejected.
func validateExternalSeedWalletConfig(cfg Config) error {
	if cfg.DataDir == "" {
		return errors.New("external-seed wallet requires an explicit " +
			"final data directory")
	}

	walletType := cfg.WalletType
	if walletType == "" && cfg.DaemonConfig != nil &&
		cfg.DaemonConfig.Wallet != nil {

		walletType = cfg.DaemonConfig.Wallet.Type
	}
	if walletType == "" {
		walletType = waved.DefaultConfig().Wallet.Type
	}
	switch walletType {
	case waved.WalletTypeLwwallet, waved.WalletTypeBtcwallet:
	default:
		return fmt.Errorf("wallet type %q cannot be initialized from "+
			"external seed entropy", walletType)
	}

	if cfg.WalletPasswordFile != "" {
		return errors.New("wallet password file is incompatible with " +
			"external-seed wallets")
	}
	if cfg.SwapDatabaseFileName != "" {
		return errors.New("custom swap database path is incompatible " +
			"with external-seed wallets")
	}
	if cfg.DaemonConfig == nil {
		return nil
	}
	if cfg.DaemonConfig.LogDirPath != "" {
		return errors.New("daemon custom log directory is " +
			"incompatible with external-seed wallets")
	}

	if cfg.DaemonConfig.Wallet != nil {
		walletCfg := cfg.DaemonConfig.Wallet
		if walletCfg.PasswordFile != "" {
			return errors.New("daemon wallet password file is " +
				"incompatible with external-seed wallets")
		}
		if walletCfg.BtcwalletDataDir != "" {
			return errors.New("custom btcwallet datadir is " +
				"incompatible with external-seed wallets")
		}
	}
	if cfg.DaemonConfig.Swap != nil &&
		cfg.DaemonConfig.Swap.DatabaseFileName != "" {
		return errors.New("daemon custom swap database path is " +
			"incompatible with external-seed wallets")
	}

	return nil
}

// verifyExternalSeedWalletIdentity checks an optional caller-persisted
// identity before returning a newly imported or unlocked wallet to the host.
func verifyExternalSeedWalletIdentity(expected, actual string) error {
	if expected == "" || expected == actual {
		return nil
	}

	return fmt.Errorf("external-seed wallet identity mismatch: expected "+
		"%s, got %s", expected, actual)
}
