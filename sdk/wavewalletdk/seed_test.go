package wavewalletdk

import (
	"errors"
	"slices"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestGenerateSeedKeepsBackupBeforeFailedInitialization proves that preparing
// words performs no InitWallet, and a later initialization error cannot remove
// the host's already-backed-up words or replace the submitted phrase.
func TestGenerateSeedKeepsBackupBeforeFailedInitialization(t *testing.T) {
	t.Parallel()

	entropy := testExternalSeedEntropy()
	words, err := entropyToMnemonic(entropy)
	require.NoError(t, err)
	defer clear(words[:])
	stub := &stubDaemonServer{
		state:     waverpc.WalletState_WALLET_STATE_NONE,
		seedWords: words[:],
		initErr:   errors.New("startup failed after database creation"),
	}
	client := newStubClient(t, stub)
	prepared, err := client.GenerateSeed(t.Context(), GenerateSeedRequest{
		SeedPassphrase: []byte("test-passphrase"),
	})
	require.NoError(t, err)
	defer clear(prepared.Mnemonic)
	require.False(t, stub.initCalled)
	require.False(t, stub.unlockCalled)
	require.Equal(
		t, []byte("test-passphrase"), stub.lastSeedReq.SeedPassphrase,
	)
	require.True(t, slices.Equal(words[:], prepared.Mnemonic))

	_, err = client.CreateWallet(t.Context(), CreateWalletRequest{
		Mnemonic:       prepared.Mnemonic,
		SeedPassphrase: []byte("test-passphrase"),
		WalletPassword: []byte("test-wallet-password"),
	})
	require.ErrorContains(t, err, "startup failed after database creation")
	require.True(t, stub.initCalled)
	require.True(t, slices.Equal(words[:], stub.lastInitReq.Mnemonic))
	require.True(t, slices.Equal(words[:], prepared.Mnemonic))
}

// TestGenerateSeedPreservesRejection proves the SDK cannot turn an existing
// wallet's GenSeed rejection into initialization or an empty success result.
func TestGenerateSeedPreservesRejection(t *testing.T) {
	t.Parallel()

	stub := &stubDaemonServer{seedErr: errors.New("wallet already exists")}
	client := newStubClient(t, stub)
	result, err := client.GenerateSeed(t.Context(), GenerateSeedRequest{})
	require.Nil(t, result)
	require.ErrorContains(t, err, "wallet already exists")
	require.False(t, stub.initCalled)
	require.False(t, stub.unlockCalled)
}
