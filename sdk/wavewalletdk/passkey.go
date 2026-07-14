package wavewalletdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/aezeed"
	"golang.org/x/crypto/hkdf"
)

// hkdfSeedInfo and hkdfDBKeyInfo domain-separate the two secrets pulled from
// one PRF output. These are derivation tags baked into the wavewalletdk
// rename: bumping them intentionally changes the derived seed/password for
// every passkey-backed wallet, so any wallet created under the old
// "walletdk" tags must be re-imported from its mnemonic rather than
// unlocked via passkey after this change.
const (
	hkdfSeedInfo  = "wavewalletdk:seed:v1"
	hkdfDBKeyInfo = "wavewalletdk:dbpw:v1"
)

// OpenWalletFromPasskey derives a reproducible wallet from a passkey's PRF
// output and either imports it (fresh device) or unlocks it (wallet already
// present locally). passkeyPRFOutput is the raw bytes from the platform's
// WebAuthn PRF evaluation; the ceremony itself lives in the platform layer.
//
// The entire wallet seed is HKDF(passkeyPRFOutput), so the caller MUST run
// the PRF ceremony with a fixed, app-controlled PRF input (the WebAuthn prf
// evaluation salt) that is identical on every device and every call for a
// given wallet. If that input ever varies, the same passkey yields a
// different seed, and the wallet and its funds become unrecoverable.
func (c *Client) OpenWalletFromPasskey(ctx context.Context,
	passkeyPRFOutput []byte) (*OpenWalletResult, error) {

	// WebAuthn PRF evaluations are at least 32 bytes. Reject anything
	// shorter so a caller bug or empty input cannot derive a fixed,
	// publicly-known seed and silently import an attacker-reproducible
	// wallet.
	if len(passkeyPRFOutput) < 32 {
		return nil, fmt.Errorf("passkey prf output too short: got %d "+
			"bytes, need at least 32", len(passkeyPRFOutput))
	}

	entropy, dbPassword := deriveSeedAndPassword(passkeyPRFOutput)

	// Read the current wallet lifecycle state so we can decide whether to
	// import a fresh seed or unlock an existing local wallet.
	info, err := c.GetInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("read wallet state: %w", err)
	}

	switch info.WalletState {
	case WalletStateNone:
		return c.createWalletFromEntropy(ctx, entropy, dbPassword)

	case WalletStateLocked:
		unlock, err := c.UnlockWallet(ctx, UnlockWalletRequest{
			WalletPassword: dbPassword,
		})
		// UnlockWallet already wraps with "unlock wallet:".
		if err != nil {
			return nil, err
		}

		return &OpenWalletResult{
			Imported:       false,
			IdentityPubKey: unlock.IdentityPubKey,
		}, nil

	case WalletStateReady, WalletStateSyncing:
		// A wallet is already open. The daemon exposes no way to
		// confirm it was derived from the presented passkey, so
		// opening it again is treated as an error. The password path
		// does the same: InitWallet and UnlockWallet both error once
		// the wallet is unlocked.
		return nil, fmt.Errorf("wallet is already unlocked")

	default:
		return nil, fmt.Errorf("unexpected wallet state %v: cannot "+
			"open wallet", info.WalletState)
	}
}

// createWalletFromEntropy builds the deterministic aezeed and initializes a new
// local wallet from it.
func (c *Client) createWalletFromEntropy(ctx context.Context,
	entropy [aezeed.EntropySize]byte, dbPassword []byte) (*OpenWalletResult,
	error) {

	mnemonic, err := entropyToMnemonic(entropy)
	if err != nil {
		return nil, fmt.Errorf("derive mnemonic: %w", err)
	}

	// The passkey seed is reproducible from entropy alone, so the seed
	// passphrase must stay empty to match deriveSeedAndPassword's contract.
	words := mnemonic[:]
	identity, err := c.initFromMnemonic(ctx, words, nil, dbPassword)
	if err != nil {
		return nil, fmt.Errorf("init wallet from mnemonic: %w", err)
	}

	return &OpenWalletResult{
		Imported:       true,
		Mnemonic:       append([]string(nil), words...),
		IdentityPubKey: identity,
	}, nil
}

// deriveSeedAndPassword expands a passkey's PRF output into the 16-byte wallet
// entropy and a local DB password. The DB password only protects the device's
// local database, so it need not match across devices; deriving it here just
// means a passkey wallet never prompts for a password.
func deriveSeedAndPassword(passkeyPRFOutput []byte) ([aezeed.EntropySize]byte,
	[]byte) {

	// HKDF-SHA256 ReadFull never errors for these fixed-size reads (both
	// outputs are well under the 255*HashLen HKDF limit), so the errors
	// are safe to ignore for both the seed and DB-password expansions.
	var entropy [aezeed.EntropySize]byte
	seedReader := hkdf.New(
		sha256.New, passkeyPRFOutput, nil, []byte(hkdfSeedInfo),
	)
	_, _ = io.ReadFull(seedReader, entropy[:])

	var raw [32]byte
	pwReader := hkdf.New(
		sha256.New, passkeyPRFOutput, nil, []byte(hkdfDBKeyInfo),
	)
	_, _ = io.ReadFull(pwReader, raw[:])

	// Encode into a preallocated slice rather than via hex.EncodeToString,
	// which would also hold the DB key as an intermediate string copy that
	// cannot be cleared and lingers until GC.
	dbPassword := make([]byte, hex.EncodedLen(len(raw)))
	hex.Encode(dbPassword, raw[:])

	return entropy, dbPassword
}

// entropyToMnemonic builds a reproducible aezeed mnemonic from fixed entropy.
// Version and birthday are pinned so the wallet depends only on the entropy.
// Note: each call returns a different 24-word string because it constructs a
// fresh aezeed with a newly drawn random salt; every such string deciphers back
// to the same entropy, and therefore the same HD wallet. Birthday sets rescan
// depth, not derived keys; pinning it avoids leaking a real creation time. The
// passphrase is empty so the seed is reproducible from entropy alone — callers
// MUST pass the same empty passphrase to InitWallet.
func entropyToMnemonic(entropy [aezeed.EntropySize]byte) (aezeed.Mnemonic,
	error) {

	cipherSeed, err := aezeed.New(0, &entropy, aezeed.BitcoinGenesisDate)
	if err != nil {
		return aezeed.Mnemonic{}, err
	}

	return cipherSeed.ToMnemonic(nil)
}
