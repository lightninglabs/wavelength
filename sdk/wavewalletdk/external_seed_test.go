package wavewalletdk

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/stretchr/testify/require"
)

// testExternalSeedEntropy returns the fixed, already-derived entropy shared by
// the external-seed unit and backend compatibility tests.
func testExternalSeedEntropy() [aezeed.EntropySize]byte {
	return [aezeed.EntropySize]byte{
		0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
		0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
	}
}

// TestWaitForExternalSeedWalletReady verifies external-seed startup delegates
// to the embedded wallet-services waiter and surfaces cancellation or startup
// errors.
func TestWaitForExternalSeedWalletReady(t *testing.T) {
	t.Parallel()

	t.Run("ready", func(t *testing.T) {
		t.Parallel()

		client := &Client{
			waitWalletServicesReady: func(context.Context) error {
				return nil
			},
		}

		require.NoError(
			t,
			waitForExternalSeedWalletReady(
				t.Context(), client,
			),
		)
	})

	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		client := &Client{
			waitWalletServicesReady: func(
				ctx context.Context) error {

				<-ctx.Done()

				return ctx.Err()
			},
		}

		require.ErrorIs(
			t, waitForExternalSeedWalletReady(ctx, client),
			context.Canceled,
		)
	})

	t.Run("startup error", func(t *testing.T) {
		t.Parallel()

		runErr := errors.New("run failed")
		client := &Client{
			waitWalletServicesReady: func(context.Context) error {
				return runErr
			},
		}

		require.ErrorIs(
			t,
			waitForExternalSeedWalletReady(
				t.Context(), client,
			),
			runErr,
		)
	})

	t.Run("remote client", func(t *testing.T) {
		t.Parallel()

		err := waitForExternalSeedWalletReady(t.Context(), &Client{})
		require.ErrorContains(t, err, "requires an embedded client")
	})
}

// TestOpenStartedExternalSeedWalletStopsOnFailure pins both rollback branches
// after Start has already returned an embedded client. The original open or
// readiness error and any shutdown error must all remain observable.
func TestOpenStartedExternalSeedWalletStopsOnFailure(t *testing.T) {
	t.Parallel()

	t.Run("open failure", func(t *testing.T) {
		t.Parallel()

		stub := &stubDaemonServer{
			state:   seedNone,
			initErr: errors.New("initialize failed"),
		}
		client := newStubClient(t, stub)
		assertExternalSeedStartRollback(
			t, client, "initialize failed",
		)
	})

	t.Run("readiness failure", func(t *testing.T) {
		t.Parallel()

		readyErr := errors.New("wallet services failed")
		stub := &stubDaemonServer{
			state: seedNone,
			initResp: &waverpc.InitWalletResponse{
				IdentityPubkey: "init-id",
			},
		}
		client := newStubClient(t, stub)
		client.waitWalletServicesReady = func(context.Context) error {
			return readyErr
		}
		assertExternalSeedStartRollback(
			t, client, "wallet services failed",
		)
	})
}

// assertExternalSeedStartRollback verifies one post-Start failure stops the
// embedded client and joins the shutdown error with the triggering failure.
func assertExternalSeedStartRollback(t *testing.T, client *Client,
	wantMessage string) {

	t.Helper()

	stopErr := errors.New("stop failed")
	stopCalls := 0
	client.closeFn = func(context.Context) error {
		stopCalls++

		return stopErr
	}

	entropy := testExternalSeedEntropy()
	result, err := openStartedExternalSeedWallet(
		t.Context(), client, ExternalSeedWalletRequest{}, entropy[:],
	)
	require.Nil(t, result)
	require.ErrorContains(t, err, wantMessage)
	require.ErrorIs(t, err, stopErr)
	require.Equal(t, 1, stopCalls)
}

// TestExternalSeedDBPasswordGoldenVector pins the password-domain version and
// HKDF transform used to reopen an existing encrypted wallet database.
func TestExternalSeedDBPasswordGoldenVector(t *testing.T) {
	t.Parallel()

	entropy := testExternalSeedEntropy()
	password := deriveExternalSeedDBPassword(entropy)
	defer clear(password)

	require.Len(t, password, 64)
	_, err := hex.DecodeString(string(password))
	require.NoError(t, err)
	require.Equal(
		t, "550ff1a159e3142b1c3b442b02e7ef2a19e780e1889dea704ce9c3d6"+
			"603189ec", string(password),
	)
}

// TestValidateExternalSeedWalletRequestRequiresAezeedEntropy rejects any
// undocumented padding, truncation, or hashing at the Go boundary.
func TestValidateExternalSeedWalletRequestRequiresAezeedEntropy(t *testing.T) {
	t.Parallel()

	require.ErrorContains(
		t, validateExternalSeedWalletRequest(nil),
		"exactly 16 bytes",
	)
	require.ErrorContains(
		t,
		validateExternalSeedWalletRequest(
			make([]byte, 15),
		),
		"exactly 16 bytes",
	)
	require.NoError(
		t,
		validateExternalSeedWalletRequest(
			make([]byte, 16),
		),
	)
	require.ErrorContains(
		t,
		validateExternalSeedWalletRequest(
			make([]byte, 17),
		),
		"exactly 16 bytes",
	)
}

// TestValidateExternalSeedWalletConfigRejectsExternalWallet verifies entropy
// is offered only to the two self-managed wallet backends that consume it.
func TestValidateExternalSeedWalletConfigRejectsExternalWallet(t *testing.T) {
	t.Parallel()

	err := validateExternalSeedWalletConfig(Config{
		DataDir:    t.TempDir(),
		WalletType: waved.WalletTypeLnd,
	})
	require.ErrorContains(t, err, "wallet type")
}

// TestStartExternalSeedWalletRejectsIdentityBeforeStartup verifies an
// identity mismatch is rejected before Start can create daemon state.
func TestStartExternalSeedWalletRejectsIdentityBeforeStartup(t *testing.T) {
	t.Setenv(wavedSeedEnvVar, "")
	t.Setenv(wavedPasswordEnvVar, "")

	dataDir := t.TempDir()
	entropy := testExternalSeedEntropy()
	client, result, err := StartExternalSeedWallet(
		t.Context(), Config{
			DataDir: dataDir,
			Network: "regtest",
		}, ExternalSeedWalletRequest{
			SeedEntropy:            entropy[:],
			ExpectedIdentityPubKey: "wrong-id",
		},
	)
	require.ErrorContains(t, err, "identity mismatch")
	require.Nil(t, client)
	require.Nil(t, result)

	entries, err := os.ReadDir(dataDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}
