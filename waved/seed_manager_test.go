package waved

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

// TestMnemonicToSeedWithBirthdayRoundtrip verifies that aezeed birthday
// metadata is preserved when deriving the raw btcwallet seed.
func TestMnemonicToSeedWithBirthdayRoundtrip(t *testing.T) {
	passphrase := []byte("test-passphrase")

	mnemonic, err := GenerateSeed(passphrase)
	require.NoError(t, err)

	seed, birthday, err := MnemonicToSeedWithBirthday(
		mnemonic, passphrase,
	)
	require.NoError(t, err)
	require.NotEqual(t, [rawSeedLen]byte{}, seed)
	require.False(t, birthday.IsZero())
	require.WithinDuration(t, time.Now(), birthday, 48*time.Hour)

	seed2, birthday2, err := MnemonicToSeedWithBirthday(
		mnemonic, passphrase,
	)
	require.NoError(t, err)
	require.Equal(t, seed, seed2)
	require.Equal(t, birthday, birthday2)
}

// TestMnemonicToSeedWrongPassphrase verifies that decoding with the
// wrong passphrase fails.
func TestMnemonicToSeedWrongPassphrase(t *testing.T) {
	mnemonic, err := GenerateSeed([]byte("correct"))
	require.NoError(t, err)

	_, err = MnemonicToSeed(mnemonic, []byte("wrong"))
	require.Error(t, err)
}

// TestLoadSeedFromEnv verifies the env var loading path.
func TestLoadSeedFromEnv(t *testing.T) {
	// Unset first — should return nil, nil.
	t.Setenv(seedEnvVar, "")
	seed, err := LoadSeedFromEnv()
	require.NoError(t, err)
	require.Nil(t, seed)

	// Set a valid 32-byte hex seed.
	hexSeed := "01020304050607080910111213141516" +
		"17181920212223242526272829303132"
	t.Setenv(seedEnvVar, hexSeed)
	seed, err = LoadSeedFromEnv()
	require.NoError(t, err)
	require.NotNil(t, seed)
	require.NotEqual(t, [rawSeedLen]byte{}, *seed)

	// Invalid hex should return an error.
	t.Setenv(seedEnvVar, "not-hex")
	_, err = LoadSeedFromEnv()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid hex seed")

	// Wrong length should return an error.
	t.Setenv(seedEnvVar, "0102030405")
	_, err = LoadSeedFromEnv()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid seed length")
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

// TestWalletSeedFromMnemonic verifies the InitWallet derivation path:
// the raw seed matches direct mnemonic decoding and the aezeed
// birthday that bounds btcwallet recovery rescans is preserved.
func TestWalletSeedFromMnemonic(t *testing.T) {
	password := []byte("workflow-password")
	passphrase := []byte("optional-passphrase")

	mnemonic, err := GenerateSeed(passphrase)
	require.NoError(t, err)

	seed, birthday, err := WalletSeedFromMnemonic(
		mnemonic[:], passphrase, password,
	)
	require.NoError(t, err)
	require.NotEqual(t, [rawSeedLen]byte{}, seed)
	require.False(t, birthday.IsZero())
	require.WithinDuration(t, time.Now(), birthday, 48*time.Hour)

	// The derived seed must match decoding the mnemonic directly,
	// so a wallet restored from the recorded words reproduces the
	// same keys.
	var recoveredMnemonic aezeed.Mnemonic
	copy(recoveredMnemonic[:], mnemonic[:])

	mnemonicSeed, err := MnemonicToSeed(recoveredMnemonic, passphrase)
	require.NoError(t, err)
	require.Equal(t, seed, mnemonicSeed)
}

// TestWalletSeedFromMnemonicValidation verifies the mnemonic word count
// and minimum password length checks.
func TestWalletSeedFromMnemonicValidation(t *testing.T) {
	passphrase := []byte("optional-passphrase")

	mnemonic, err := GenerateSeed(passphrase)
	require.NoError(t, err)

	// Too few mnemonic words.
	_, _, err = WalletSeedFromMnemonic(
		mnemonic[:12], passphrase, []byte("workflow-password"),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mnemonic must be")

	// Password below the minimum length.
	_, _, err = WalletSeedFromMnemonic(
		mnemonic[:], passphrase, []byte("short"),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "at least")

	// Wrong seed passphrase fails decoding.
	_, _, err = WalletSeedFromMnemonic(
		mnemonic[:], []byte("wrong"), []byte("workflow-password"),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid mnemonic")
}
