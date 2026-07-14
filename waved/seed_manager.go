package waved

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lightninglabs/wavelength/walletcore"
	"github.com/lightningnetwork/lnd/aezeed"
)

const (
	// seedEnvVar is the environment variable name for providing a
	// hex-encoded raw seed directly. This bypasses the aezeed
	// mnemonic flow and is intended for development and CI
	// environments.
	seedEnvVar = "WAVED_LWWALLET_SEED"

	// walletPasswordEnvVar is the environment variable name for
	// providing the wallet password for auto-unlock at daemon
	// startup.
	walletPasswordEnvVar = "WAVED_WALLET_PASSWORD" //nolint:gosec

	// rawSeedLen is the expected length of the raw HD seed in bytes.
	rawSeedLen = walletcore.SeedLen

	// minPasswordLen is the minimum wallet password length in bytes.
	minPasswordLen = 8
)

// zeroBytes overwrites a byte slice with zeros. This is used to clear
// sensitive key material from memory after use. The noinline directive
// prevents the compiler from optimizing away the dead store when the
// buffer is not read after zeroing.
//
//go:noinline
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// devWalletPassword is the insecure fallback private passphrase for
// wallets created through the WAVED_LWWALLET_SEED dev/CI path when
// WAVED_WALLET_PASSWORD is not also set. It is never used for
// operator-created wallets: the InitWallet and UnlockWallet RPCs
// always require an explicit password.
var devWalletPassword = []byte("dev-insecure-password")

// GenerateSeed generates a new aezeed cipher seed and returns its
// 24-word mnemonic representation. The optional passphrase protects
// the mnemonic itself (distinct from the wallet password that encrypts
// the wallet database).
func GenerateSeed(passphrase []byte) (aezeed.Mnemonic, error) {
	var entropy [aezeed.EntropySize]byte

	if _, err := rand.Read(entropy[:]); err != nil {
		return aezeed.Mnemonic{}, fmt.Errorf("reading entropy: %w", err)
	}

	cipherSeed, err := aezeed.New(
		aezeed.CipherSeedVersion, &entropy, time.Now(),
	)
	if err != nil {
		return aezeed.Mnemonic{}, fmt.Errorf("creating cipher seed: %w",
			err)
	}

	mnemonic, err := cipherSeed.ToMnemonic(passphrase)
	if err != nil {
		return aezeed.Mnemonic{}, fmt.Errorf("encoding mnemonic: %w",
			err)
	}

	return mnemonic, nil
}

// MnemonicToSeed decodes an aezeed mnemonic and extracts the raw
// 32-byte HD seed from its entropy field. The passphrase must match
// the one used during mnemonic generation.
func MnemonicToSeed(mnemonic aezeed.Mnemonic,
	passphrase []byte) ([rawSeedLen]byte, error) {

	seed, _, err := MnemonicToSeedWithBirthday(mnemonic, passphrase)

	return seed, err
}

// MnemonicToSeedWithBirthday decodes an aezeed mnemonic and returns both the
// raw HD seed entropy and the aezeed birthday. The birthday bounds btcwallet
// recovery scans so freshly created wallets do not rescan from genesis.
func MnemonicToSeedWithBirthday(mnemonic aezeed.Mnemonic, passphrase []byte) (
	[rawSeedLen]byte, time.Time, error) {

	cipherSeed, err := mnemonic.ToCipherSeed(passphrase)
	if err != nil {
		return [rawSeedLen]byte{}, time.Time{}, fmt.Errorf(
			"deciphering mnemonic: %w", err)
	}

	var seed [rawSeedLen]byte
	copy(seed[:], cipherSeed.Entropy[:])

	return seed, cipherSeed.BirthdayTime(), nil
}

// LoadSeedFromEnv reads a hex-encoded raw seed from the
// WAVED_LWWALLET_SEED environment variable. It returns the seed if
// the variable is set and valid. It returns (nil, nil) if the variable
// is not set. It returns an error if the variable is set but invalid.
func LoadSeedFromEnv() (*[rawSeedLen]byte, error) {
	hexSeed := os.Getenv(seedEnvVar)
	if hexSeed == "" {
		return nil, nil
	}

	decoded, err := hex.DecodeString(strings.TrimSpace(hexSeed))
	if err != nil {
		return nil, fmt.Errorf("invalid hex seed in %s: %w", seedEnvVar,
			err)
	}

	if len(decoded) != rawSeedLen {
		return nil, fmt.Errorf("invalid seed length in %s: got "+
			"%d, want %d", seedEnvVar, len(decoded), rawSeedLen)
	}

	var seed [rawSeedLen]byte
	copy(seed[:], decoded)

	return &seed, nil
}

// LoadPasswordFromEnv reads the wallet password from the
// WAVED_WALLET_PASSWORD environment variable.
func LoadPasswordFromEnv() ([]byte, bool) {
	pass := os.Getenv(walletPasswordEnvVar)
	if pass == "" {
		return nil, false
	}

	return []byte(pass), true
}

// LoadPasswordFromFile reads a wallet password from a file, stripping
// any trailing newline characters.
func LoadPasswordFromFile(path string) ([]byte, error) {
	// The path is an operator-supplied config value; variable path is
	// the intended API.
	data, err := os.ReadFile(path) //nolint:gosec // G304
	if err != nil {
		return nil, fmt.Errorf("reading password file %q: %w", path,
			err)
	}

	// Strip trailing newlines so users can create the file with
	// echo without worrying about the trailing newline.
	password := []byte(strings.TrimRight(string(data), "\n\r"))

	return password, nil
}
