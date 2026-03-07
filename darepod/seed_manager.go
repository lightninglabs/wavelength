package darepod

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/aezeed"
	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/scrypt"
)

const (
	// seedFileBaseName is the name of the encrypted seed file stored
	// in the network data directory.
	seedFileBaseName = "wallet_seed.enc"

	// seedEnvVar is the environment variable name for providing a
	// hex-encoded raw seed directly. This bypasses the encrypted
	// seed file and is intended for development and CI environments.
	seedEnvVar = "DAREPOD_LWWALLET_SEED"

	// walletPasswordEnvVar is the environment variable name for
	// providing the wallet password for auto-unlock at daemon
	// startup.
	walletPasswordEnvVar = "DAREPOD_WALLET_PASSWORD"

	// scryptN is the CPU/memory cost parameter for scrypt key
	// derivation. This value balances security against startup
	// latency.
	scryptN = 1 << 18

	// scryptR is the block size parameter for scrypt.
	scryptR = 8

	// scryptP is the parallelism parameter for scrypt.
	scryptP = 1

	// scryptKeyLen is the derived key length in bytes. We need 32
	// bytes for the NaCl secretbox key.
	scryptKeyLen = 32

	// scryptSaltLen is the length of the random salt stored
	// alongside the ciphertext.
	scryptSaltLen = 32

	// secretboxNonceLen is the NaCl secretbox nonce length.
	secretboxNonceLen = 24

	// rawSeedLen is the expected length of the raw HD seed in bytes.
	rawSeedLen = 32

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

// GenerateSeed generates a new aezeed cipher seed and returns its
// 24-word mnemonic representation. The optional passphrase protects
// the mnemonic itself (distinct from the wallet password that encrypts
// the seed at rest).
func GenerateSeed(passphrase []byte) (aezeed.Mnemonic, error) {
	var entropy [aezeed.EntropySize]byte

	if _, err := rand.Read(entropy[:]); err != nil {
		return aezeed.Mnemonic{}, fmt.Errorf(
			"reading entropy: %w", err,
		)
	}

	cipherSeed, err := aezeed.New(
		aezeed.CipherSeedVersion, &entropy, time.Now(),
	)
	if err != nil {
		return aezeed.Mnemonic{}, fmt.Errorf(
			"creating cipher seed: %w", err,
		)
	}

	mnemonic, err := cipherSeed.ToMnemonic(passphrase)
	if err != nil {
		return aezeed.Mnemonic{}, fmt.Errorf(
			"encoding mnemonic: %w", err,
		)
	}

	return mnemonic, nil
}

// MnemonicToSeed decodes an aezeed mnemonic and extracts the raw
// 32-byte HD seed from its entropy field. The passphrase must match
// the one used during mnemonic generation.
func MnemonicToSeed(mnemonic aezeed.Mnemonic,
	passphrase []byte) ([rawSeedLen]byte, error) {

	cipherSeed, err := mnemonic.ToCipherSeed(passphrase)
	if err != nil {
		return [rawSeedLen]byte{}, fmt.Errorf(
			"deciphering mnemonic: %w", err,
		)
	}

	var seed [rawSeedLen]byte
	copy(seed[:], cipherSeed.Entropy[:])

	return seed, nil
}

// EncryptSeed encrypts a raw seed using scrypt for key derivation and
// NaCl secretbox for authenticated encryption. The returned ciphertext
// includes the scrypt salt and secretbox nonce as a prefix, so it is
// self-contained for decryption.
//
// Format: salt (32 bytes) || nonce (24 bytes) || ciphertext (variable).
func EncryptSeed(seed [rawSeedLen]byte,
	password []byte) ([]byte, error) {

	if len(password) < minPasswordLen {
		return nil, fmt.Errorf(
			"password must be at least %d bytes",
			minPasswordLen,
		)
	}

	// Derive encryption key from password via scrypt.
	salt := make([]byte, scryptSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating salt: %w", err)
	}

	key, err := scrypt.Key(
		password, salt, scryptN, scryptR, scryptP, scryptKeyLen,
	)
	if err != nil {
		return nil, fmt.Errorf("deriving key: %w", err)
	}
	defer zeroBytes(key)

	var secretKey [scryptKeyLen]byte
	copy(secretKey[:], key)
	defer zeroBytes(secretKey[:])

	// Generate random nonce for secretbox.
	var nonce [secretboxNonceLen]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}

	// Encrypt the seed. The secretbox Seal function appends the
	// authenticated ciphertext to the output slice.
	encrypted := secretbox.Seal(nil, seed[:], &nonce, &secretKey)

	// Assemble the output: salt || nonce || ciphertext.
	result := make(
		[]byte, 0,
		scryptSaltLen+secretboxNonceLen+len(encrypted),
	)
	result = append(result, salt...)
	result = append(result, nonce[:]...)
	result = append(result, encrypted...)

	return result, nil
}

// DecryptSeed reverses EncryptSeed: it extracts the salt and nonce
// from the ciphertext prefix, derives the key via scrypt, and decrypts
// the seed using NaCl secretbox.
func DecryptSeed(ciphertext,
	password []byte) ([rawSeedLen]byte, error) {

	minLen := scryptSaltLen + secretboxNonceLen + secretbox.Overhead +
		rawSeedLen

	if len(ciphertext) < minLen {
		return [rawSeedLen]byte{}, fmt.Errorf(
			"ciphertext too short: got %d, need at least %d",
			len(ciphertext), minLen,
		)
	}

	// Extract salt, nonce, and encrypted payload.
	salt := ciphertext[:scryptSaltLen]
	var nonce [secretboxNonceLen]byte
	copy(
		nonce[:],
		ciphertext[scryptSaltLen:scryptSaltLen+secretboxNonceLen],
	)
	box := ciphertext[scryptSaltLen+secretboxNonceLen:]

	// Derive the decryption key.
	key, err := scrypt.Key(
		password, salt, scryptN, scryptR, scryptP, scryptKeyLen,
	)
	if err != nil {
		return [rawSeedLen]byte{}, fmt.Errorf(
			"deriving key: %w", err,
		)
	}
	defer zeroBytes(key)

	var secretKey [scryptKeyLen]byte
	copy(secretKey[:], key)
	defer zeroBytes(secretKey[:])

	// Decrypt and authenticate.
	plaintext, ok := secretbox.Open(nil, box, &nonce, &secretKey)
	if !ok {
		return [rawSeedLen]byte{}, fmt.Errorf(
			"decryption failed: wrong password or " +
				"corrupted seed file",
		)
	}
	defer zeroBytes(plaintext)

	if len(plaintext) != rawSeedLen {
		return [rawSeedLen]byte{}, fmt.Errorf(
			"unexpected seed length: got %d, want %d",
			len(plaintext), rawSeedLen,
		)
	}

	var seed [rawSeedLen]byte
	copy(seed[:], plaintext)

	return seed, nil
}

// LoadSeedFromEnv reads a hex-encoded raw seed from the
// DAREPOD_LWWALLET_SEED environment variable. It returns the seed if
// the variable is set and valid. It returns (nil, nil) if the variable
// is not set. It returns an error if the variable is set but invalid.
func LoadSeedFromEnv() (*[rawSeedLen]byte, error) {
	hexSeed := os.Getenv(seedEnvVar)
	if hexSeed == "" {
		return nil, nil
	}

	decoded, err := hex.DecodeString(strings.TrimSpace(hexSeed))
	if err != nil {
		return nil, fmt.Errorf(
			"invalid hex seed in %s: %w",
			seedEnvVar, err,
		)
	}

	if len(decoded) != rawSeedLen {
		return nil, fmt.Errorf(
			"invalid seed length in %s: got %d, want %d",
			seedEnvVar, len(decoded), rawSeedLen,
		)
	}

	var seed [rawSeedLen]byte
	copy(seed[:], decoded)

	return &seed, nil
}

// LoadPasswordFromEnv reads the wallet password from the
// DAREPOD_WALLET_PASSWORD environment variable.
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
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(
			"reading password file %q: %w", path, err,
		)
	}

	// Strip trailing newlines so users can create the file with
	// echo without worrying about the trailing newline.
	password := []byte(strings.TrimRight(string(data), "\n\r"))

	return password, nil
}

// SeedFilePath returns the path to the encrypted seed file within the
// given network data directory.
func SeedFilePath(networkDir string) string {
	return filepath.Join(networkDir, seedFileBaseName)
}

// SaveEncryptedSeed writes the encrypted seed ciphertext to disk at
// the given path. The file is created with restrictive permissions
// (0600) to prevent unauthorized access.
func SaveEncryptedSeed(path string, ciphertext []byte) error {
	// Ensure the parent directory exists.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating directory %q: %w", dir, err)
	}

	if err := os.WriteFile(path, ciphertext, 0600); err != nil {
		return fmt.Errorf(
			"writing seed file %q: %w", path, err,
		)
	}

	return nil
}

// LoadEncryptedSeed reads the encrypted seed ciphertext from disk. It
// returns an error if the file does not exist or cannot be read.
func LoadEncryptedSeed(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(
			"reading seed file %q: %w", path, err,
		)
	}

	return data, nil
}

// SeedFileExists returns true if an encrypted seed file exists at the
// expected path within the network data directory.
func SeedFileExists(networkDir string) bool {
	path := SeedFilePath(networkDir)

	_, err := os.Stat(path)

	return err == nil
}
