package darepod

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/stretchr/testify/require"
)

// TestGenerateSeedProduces24Words verifies that GenerateSeed returns a
// valid 24-word mnemonic.
func TestGenerateSeedProduces24Words(t *testing.T) {
	mnemonic, err := GenerateSeed(nil)
	require.NoError(t, err)

	for i, word := range mnemonic {
		require.NotEmpty(t, word, "word %d is empty", i)
	}
}

// TestMnemonicToSeedRoundtrip verifies that a mnemonic can be decoded
// back to the same seed entropy.
func TestMnemonicToSeedRoundtrip(t *testing.T) {
	passphrase := []byte("test-passphrase")

	mnemonic, err := GenerateSeed(passphrase)
	require.NoError(t, err)

	seed, err := MnemonicToSeed(mnemonic, passphrase)
	require.NoError(t, err)
	require.NotEqual(t, [rawSeedLen]byte{}, seed)

	// Decode again to verify determinism.
	seed2, err := MnemonicToSeed(mnemonic, passphrase)
	require.NoError(t, err)
	require.Equal(t, seed, seed2)
}

// TestMnemonicToSeedWrongPassphrase verifies that decoding with the
// wrong passphrase fails.
func TestMnemonicToSeedWrongPassphrase(t *testing.T) {
	mnemonic, err := GenerateSeed([]byte("correct"))
	require.NoError(t, err)

	_, err = MnemonicToSeed(mnemonic, []byte("wrong"))
	require.Error(t, err)
}

// TestEncryptDecryptRoundtrip verifies that a seed survives
// encrypt-then-decrypt.
func TestEncryptDecryptRoundtrip(t *testing.T) {
	password := []byte("a-secure-password")

	// Generate a seed.
	mnemonic, err := GenerateSeed(nil)
	require.NoError(t, err)

	seed, err := MnemonicToSeed(mnemonic, nil)
	require.NoError(t, err)

	// Encrypt.
	ciphertext, err := EncryptSeed(seed, password)
	require.NoError(t, err)
	require.NotEmpty(t, ciphertext)

	// Decrypt.
	recovered, err := DecryptSeed(ciphertext, password)
	require.NoError(t, err)
	require.Equal(t, seed, recovered)
}

// TestDecryptWrongPassword verifies that decryption with the wrong
// password fails cleanly.
func TestDecryptWrongPassword(t *testing.T) {
	password := []byte("correct-password")
	var seed [rawSeedLen]byte
	copy(seed[:], []byte("deterministic-test-seed-value!!!"))

	ciphertext, err := EncryptSeed(seed, password)
	require.NoError(t, err)

	_, err = DecryptSeed(ciphertext, []byte("wrong-password!!"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "wrong password")
}

// TestEncryptSeedPasswordTooShort verifies the minimum password length
// check.
func TestEncryptSeedPasswordTooShort(t *testing.T) {
	var seed [rawSeedLen]byte

	_, err := EncryptSeed(seed, []byte("short"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "at least")
}

// TestSaveLoadEncryptedSeed verifies file I/O for encrypted seeds.
func TestSaveLoadEncryptedSeed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test_seed.enc")
	data := []byte("test-ciphertext-data")

	err := SaveEncryptedSeed(path, data)
	require.NoError(t, err)

	loaded, err := LoadEncryptedSeed(path)
	require.NoError(t, err)
	require.Equal(t, data, loaded)

	// Verify restrictive permissions.
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

// TestSeedFileExists verifies the existence check.
func TestSeedFileExists(t *testing.T) {
	dir := t.TempDir()

	require.False(t, SeedFileExists(dir))

	path := SeedFilePath(dir)
	err := SaveEncryptedSeed(path, []byte("data"))
	require.NoError(t, err)

	require.True(t, SeedFileExists(dir))
}

// TestLoadSeedFromEnv verifies the env var loading path.
func TestLoadSeedFromEnv(t *testing.T) {
	// Unset first.
	t.Setenv(seedEnvVar, "")
	_, ok := LoadSeedFromEnv()
	require.False(t, ok)

	// Set a valid 32-byte hex seed.
	hexSeed := "0102030405060708091011121314151617181920212223242526272829303132"
	t.Setenv(seedEnvVar, hexSeed)
	seed, ok := LoadSeedFromEnv()
	require.True(t, ok)
	require.NotEqual(t, [rawSeedLen]byte{}, seed)

	// Invalid hex.
	t.Setenv(seedEnvVar, "not-hex")
	_, ok = LoadSeedFromEnv()
	require.False(t, ok)

	// Wrong length.
	t.Setenv(seedEnvVar, "0102030405")
	_, ok = LoadSeedFromEnv()
	require.False(t, ok)
}

// TestLoadPasswordFromFile verifies reading passwords from files.
func TestLoadPasswordFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "password.txt")

	// With trailing newline.
	err := os.WriteFile(path, []byte("my-password\n"), 0600)
	require.NoError(t, err)

	password, err := LoadPasswordFromFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("my-password"), password)

	// Without trailing newline.
	err = os.WriteFile(path, []byte("clean-pass"), 0600)
	require.NoError(t, err)

	password, err = LoadPasswordFromFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("clean-pass"), password)
}

// TestFullWorkflow exercises the complete create-save-load-unlock flow.
func TestFullWorkflow(t *testing.T) {
	dir := t.TempDir()
	password := []byte("workflow-password")
	passphrase := []byte("optional-passphrase")

	// Step 1: Generate seed.
	mnemonic, err := GenerateSeed(passphrase)
	require.NoError(t, err)

	// Step 2: Derive raw seed from mnemonic.
	seed, err := MnemonicToSeed(mnemonic, passphrase)
	require.NoError(t, err)

	// Step 3: Encrypt and save.
	ciphertext, err := EncryptSeed(seed, password)
	require.NoError(t, err)

	seedPath := SeedFilePath(dir)
	err = SaveEncryptedSeed(seedPath, ciphertext)
	require.NoError(t, err)

	// Step 4: Load and decrypt (simulates daemon restart).
	loaded, err := LoadEncryptedSeed(seedPath)
	require.NoError(t, err)

	recovered, err := DecryptSeed(loaded, password)
	require.NoError(t, err)
	require.Equal(t, seed, recovered)

	// Verify we can also recover from the mnemonic words directly.
	var recoveredMnemonic aezeed.Mnemonic
	copy(recoveredMnemonic[:], mnemonic[:])

	mnemonicSeed, err := MnemonicToSeed(recoveredMnemonic, passphrase)
	require.NoError(t, err)
	require.Equal(t, seed, mnemonicSeed)
}
