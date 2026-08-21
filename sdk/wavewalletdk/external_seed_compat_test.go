package wavewalletdk

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/stretchr/testify/require"
)

type seedHarness struct {
	entropy       [aezeed.EntropySize]byte
	stub          *stubDaemonServer
	client        *Client
	recoveryErr   error
	recoveryCalls int
}

// newSeedHarness wires a stub wallet and optional recovery bridge.
func newSeedHarness(t *testing.T, state waverpc.WalletState, identity string,
	withRecovery bool) *seedHarness {

	stub := &stubDaemonServer{
		state:    state,
		identity: identity,
		initResp: &waverpc.InitWalletResponse{
			IdentityPubkey: identity,
		},
	}
	h := &seedHarness{
		entropy: testExternalSeedEntropy(),
		stub:    stub,
		client:  newStubClient(t, stub),
	}
	if !withRecovery {
		return h
	}

	h.client.recoverWalletState = func(_ context.Context, window uint32) (
		*waved.WalletRecoveryResult, error) {

		h.recoveryCalls++
		require.Equal(t, uint32(144), window)
		require.True(t, stub.initCalled || stub.unlockCalled)
		if h.recoveryErr != nil {
			return nil, h.recoveryErr
		}

		return &waved.WalletRecoveryResult{
			BoardingAddresses: 1,
			BoardingUTXOs:     2,
			VTXOs:             3,
			OORReceiveScripts: 4,
			OOREvents:         5,
		}, nil
	}

	return h
}

// open sends the harness entropy through the private external-seed opener.
func (h *seedHarness) open(t *testing.T, expected string, runRecovery bool) (
	*ExternalSeedWalletOpenResult, error) {

	t.Helper()

	return h.client.openWalletFromExternalSeed(
		t.Context(), ExternalSeedWalletRequest{
			SeedEntropy:            h.entropy[:],
			ExpectedIdentityPubKey: expected,
			RecoverState:           runRecovery,
			RecoveryWindow:         144,
		},
	)
}

// recoveredResult returns the result expected from the recovery fixture.
func recoveredResult(imported bool,
	identity string) *ExternalSeedWalletOpenResult {

	return &ExternalSeedWalletOpenResult{
		Imported:                    imported,
		IdentityPubKey:              identity,
		RecoveryRan:                 true,
		RecoveredBoardingAddresses:  1,
		RecoveredBoardingUTXOs:      2,
		RecoveredVTXOs:              3,
		RecoveredOORReceiveScripts:  4,
		RecoveredOORRecipientEvents: 5,
	}
}

// requireSeedStartError checks validation before filesystem writes.
func requireSeedStartError(t *testing.T, dataDir string, seed []byte,
	want string) {

	t.Helper()
	cfg := Config{DataDir: dataDir}
	client, result, err := StartExternalSeedWallet(
		t.Context(), cfg, ExternalSeedWalletRequest{
			SeedEntropy: seed,
		},
	)
	require.ErrorContains(t, err, want)
	require.Nil(t, client)
	require.Nil(t, result)
	if dataDir == "" {
		return
	}

	entries, err := os.ReadDir(dataDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

// TestOpenWalletFromExternalSeed verifies import, unlock, and recovery.
func TestOpenWalletFromExternalSeed(t *testing.T) {
	t.Parallel()
	t.Run("fresh import and recovery", testExternalSeedImport)
	t.Run("existing wallet unlock", testExternalSeedUnlock)
}

// testExternalSeedImport verifies the fresh-wallet import contract.
func testExternalSeedImport(t *testing.T) {
	t.Parallel()
	h := newSeedHarness(
		t, waverpc.WalletState_WALLET_STATE_NONE, "init-id", true,
	)
	result, err := h.open(t, "init-id", true)
	require.NoError(t, err)
	require.True(t, h.stub.initCalled)
	require.False(t, h.stub.unlockCalled)
	require.Equal(t, 1, h.recoveryCalls)
	require.Equal(t, recoveredResult(true, "init-id"), result)

	password := deriveExternalSeedDBPassword(h.entropy)
	require.False(t, h.stub.lastInitReq.RecoverState)
	require.Zero(t, h.stub.lastInitReq.RecoveryWindow)
	require.Empty(t, h.stub.lastInitReq.SeedPassphrase)
	require.Equal(t, password, h.stub.lastInitReq.WalletPassword)

	var mnemonic aezeed.Mnemonic
	copy(mnemonic[:], h.stub.lastInitReq.Mnemonic)
	cipherSeed, err := mnemonic.ToCipherSeed(nil)
	require.NoError(t, err)
	require.Equal(t, h.entropy, cipherSeed.Entropy)
	require.Zero(t, cipherSeed.Birthday)
}

// testExternalSeedUnlock verifies deterministic existing-wallet unlock.
func testExternalSeedUnlock(t *testing.T) {
	t.Parallel()
	h := newSeedHarness(
		t, waverpc.WalletState_WALLET_STATE_LOCKED, "unlock-id", true,
	)
	result, err := h.open(t, "unlock-id", false)
	require.NoError(t, err)
	require.False(t, h.stub.initCalled)
	require.True(t, h.stub.unlockCalled)
	require.Zero(t, h.recoveryCalls)
	require.Equal(t, &ExternalSeedWalletOpenResult{
		IdentityPubKey: "unlock-id",
	}, result)
	require.Equal(
		t, deriveExternalSeedDBPassword(h.entropy),
		h.stub.lastUnlockReq.WalletPassword,
	)
}

// TestOpenWalletFromExternalSeedRetriesRecoveryAfterFailedImport tests retry.
func TestOpenWalletFromExternalSeedRetriesRecoveryAfterFailedImport(
	t *testing.T) {

	t.Parallel()
	first := newSeedHarness(
		t, waverpc.WalletState_WALLET_STATE_NONE, "unlock-id", true,
	)
	first.recoveryErr = errors.New("state recovery failed")
	_, err := first.open(t, "unlock-id", true)
	require.ErrorContains(t, err, "state recovery failed")
	require.True(t, first.stub.initCalled)
	require.False(t, first.stub.lastInitReq.RecoverState)

	retry := newSeedHarness(
		t, waverpc.WalletState_WALLET_STATE_LOCKED, "unlock-id", true,
	)
	result, err := retry.open(t, "unlock-id", true)
	require.NoError(t, err)
	require.Equal(t, 1, retry.recoveryCalls)
	require.Equal(t, recoveredResult(false, "unlock-id"), result)
}

type externalSeedOpenGuard uint8

const (
	seedNone    = waverpc.WalletState_WALLET_STATE_NONE
	seedLocked  = waverpc.WalletState_WALLET_STATE_LOCKED
	seedReady   = waverpc.WalletState_WALLET_STATE_READY
	seedSyncing = waverpc.WalletState_WALLET_STATE_SYNCING
)

const (
	needsBridge externalSeedOpenGuard = iota
	identityMismatch
	identityBeforeRecovery
	alreadyUnlocked
	invalidEntropy
)

// TestOpenWalletFromExternalSeedRejectsUnsafeOpens covers open guards.
func TestOpenWalletFromExternalSeedRejectsUnsafeOpens(t *testing.T) {
	t.Parallel()
	testExternalSeedGuard(t, "fresh needs bridge", seedNone, needsBridge)
	testExternalSeedGuard(
		t, "restart needs bridge", seedLocked, needsBridge,
	)
	testExternalSeedGuard(
		t, "fresh identity before recovery", seedNone,
		identityBeforeRecovery,
	)
	testExternalSeedGuard(
		t, "restart identity before recovery", seedLocked,
		identityBeforeRecovery,
	)
	testExternalSeedGuard(
		t, "fresh identity mismatch", seedNone, identityMismatch,
	)
	testExternalSeedGuard(
		t, "restart identity mismatch", seedLocked, identityMismatch,
	)
	testExternalSeedGuard(t, "ready wallet", seedReady, alreadyUnlocked)
	testExternalSeedGuard(t, "syncing wallet", seedSyncing, alreadyUnlocked)
	testExternalSeedGuard(t, "wrong entropy size", seedNone, invalidEntropy)
}

// testExternalSeedGuard verifies one unsafe-open scenario.
func testExternalSeedGuard(t *testing.T, name string, state waverpc.WalletState,
	guard externalSeedOpenGuard) {

	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Parallel()
		runRecovery := guard == identityBeforeRecovery
		h := newSeedHarness(t, state, "actual-id", runRecovery)
		var err error
		var want string
		switch guard {
		case needsBridge:
			_, err = h.open(t, "", true)
			want = "requires an embedded client"

		case identityMismatch, identityBeforeRecovery:
			_, err = h.open(t, "wrong-id", runRecovery)
			want = "identity"

		case alreadyUnlocked:
			_, err = h.open(t, "actual-id", false)
			want = "already unlocked"

		case invalidEntropy:
			_, err = h.client.openWalletFromExternalSeed(
				t.Context(), ExternalSeedWalletRequest{
					SeedEntropy: bytes.Repeat(
						[]byte{1}, aezeed.EntropySize+1,
					),
				},
			)
			want = "exactly 16 bytes"
		}

		require.ErrorContains(t, err, want)
		identityGuard := guard == identityMismatch || runRecovery
		require.Equal(
			t, identityGuard && state == seedNone,
			h.stub.initCalled,
		)
		require.Equal(
			t, identityGuard && state == seedLocked,
			h.stub.unlockCalled,
		)
		require.Zero(t, h.recoveryCalls)
		if h.stub.initCalled {
			require.False(t, h.stub.lastInitReq.RecoverState)
		}
	})
}

// TestStartExternalSeedWalletValidatesBeforeStartup tests early validation.
func TestStartExternalSeedWalletValidatesBeforeStartup(t *testing.T) {
	t.Setenv(wavedSeedEnvVar, "")
	t.Setenv(wavedPasswordEnvVar, "")
	requireSeedStartError(
		t, t.TempDir(), make([]byte, aezeed.EntropySize-1),
		"exactly 16 bytes",
	)
	entropy := testExternalSeedEntropy()
	requireSeedStartError(
		t, "", entropy[:], "explicit final data directory",
	)
}

// TestExternalSeedWalletConfigUsesExactDataDir tests caller-owned paths.
func TestExternalSeedWalletConfigUsesExactDataDir(t *testing.T) {
	t.Parallel()
	legacy, err := daemonConfig(Config{})
	require.NoError(t, err)
	require.Equal(t, waved.DefaultConfig().DataDir, legacy.DataDir)

	daemonCfg := waved.DefaultConfig()
	daemonCfg.DataDir = t.TempDir()
	originalDir, originalType := daemonCfg.DataDir, daemonCfg.Wallet.Type
	finalDir := filepath.Join(t.TempDir(), "sdk-selected-wallet")
	cfg := Config{
		DaemonConfig: daemonCfg,
		DataDir:      finalDir,
		Network:      "regtest",
		WalletType:   waved.WalletTypeBtcwallet,
	}
	require.NoError(t, validateExternalSeedWalletConfig(cfg))
	resolved, err := daemonConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, finalDir, resolved.DataDir)
	require.Equal(t, waved.WalletTypeBtcwallet, resolved.Wallet.Type)
	require.Equal(t, originalDir, daemonCfg.DataDir)
	require.Equal(t, originalType, daemonCfg.Wallet.Type)

	missingDir := Config{DaemonConfig: daemonCfg}
	require.ErrorContains(
		t, validateExternalSeedWalletConfig(missingDir),
		"explicit final data directory",
	)
}

// TestStartExternalSeedWalletRejectsProcessGlobals tests legacy hook guards.
func TestStartExternalSeedWalletRejectsProcessGlobals(t *testing.T) {
	entropy := testExternalSeedEntropy()
	original := bytes.Clone(entropy[:])
	testSeedProcessGlobal(
		t, wavedSeedEnvVar, "000102030405060708090a0b0c0d0e0f1011121"+
			"31415161718191a1b1c1d1e1f", entropy[:],
	)
	testSeedProcessGlobal(
		t, wavedPasswordEnvVar, "process-global-password", entropy[:],
	)
	require.Equal(t, original, entropy[:])
}

// testSeedProcessGlobal verifies one process-global guard.
func testSeedProcessGlobal(t *testing.T, envVar, value string, entropy []byte) {
	t.Helper()
	t.Run(envVar, func(t *testing.T) {
		t.Setenv(wavedSeedEnvVar, "")
		t.Setenv(wavedPasswordEnvVar, "")
		t.Setenv(envVar, value)
		requireSeedStartError(
			t, t.TempDir(), entropy, envVar,
		)
	})
}

type externalSeedConfigOverride uint8

const (
	overrideWalletPassword externalSeedConfigOverride = iota
	overrideSwapDatabase
	overrideLogDirectory
	overrideDaemonPassword
	overrideBtcwalletDatabase
	overrideDaemonSwapDatabase
)

// TestExternalSeedWalletConfigRejectsStateOverrides tests path isolation.
func TestExternalSeedWalletConfigRejectsStateOverrides(t *testing.T) {
	t.Parallel()
	for override := overrideWalletPassword; override <=
		overrideDaemonSwapDatabase; override++ {

		testExternalSeedConfigOverride(t, override)
	}
}

// testExternalSeedConfigOverride verifies one alternate state path is rejected.
func testExternalSeedConfigOverride(t *testing.T,
	override externalSeedConfigOverride) {

	t.Helper()
	cfg := Config{
		DaemonConfig: waved.DefaultConfig(),
		DataDir:      "wallet",
	}
	var name, want string
	switch override {
	case overrideWalletPassword:
		name, want = "wallet password file", "password file"
		cfg.WalletPasswordFile = "wallet.pw"

	case overrideSwapDatabase:
		name, want = "swap database", "database"
		cfg.SwapDatabaseFileName = "swaps.db"

	case overrideLogDirectory:
		name, want = "daemon log directory", "log directory"
		cfg.DaemonConfig.LogDirPath = "logs"

	case overrideDaemonPassword:
		name, want = "daemon wallet password file", "password file"
		cfg.DaemonConfig.Wallet.PasswordFile = "wallet.pw"

	case overrideBtcwalletDatabase:
		name, want = "btcwallet database", "datadir"
		cfg.WalletType = waved.WalletTypeBtcwallet
		cfg.DaemonConfig.Wallet.BtcwalletDataDir = "btcwallet"

	case overrideDaemonSwapDatabase:
		name, want = "daemon swap database", "database"
		cfg.DaemonConfig.Swap.DatabaseFileName = "swaps.db"
	}

	t.Run(name, func(t *testing.T) {
		t.Parallel()
		require.ErrorContains(
			t, validateExternalSeedWalletConfig(cfg), want,
		)
	})
}
