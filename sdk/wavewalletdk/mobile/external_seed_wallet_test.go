//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
	"github.com/stretchr/testify/require"
)

// resetMobileState isolates tests that exercise the package singleton.
func resetMobileState(t *testing.T) {
	t.Helper()
	require.NoError(t, Stop())
	t.Cleanup(func() { _ = Stop() })
}

// requireContextDone checks a synchronous context cancellation boundary.
func requireContextDone(t *testing.T, ctx context.Context, want bool) {
	t.Helper()

	select {
	case <-ctx.Done():
		require.True(t, want, "context unexpectedly cancelled")

	default:
		require.False(t, want, "context was not cancelled")
	}
}

// TestParseExternalSeedWalletStartRequest verifies the private wire contract.
func TestParseExternalSeedWalletStartRequest(t *testing.T) {
	cfg, req, err := parseExternalSeedWalletStartRequest([]byte(`{
		"config": {
			"data_dir": "/tmp/wavelength-wallet",
			"network": "regtest",
			"wallet_type": "btcwallet"
		},
		"seed_entropy": "ABEiM0RVZneImaq7zN3u/w==",
		"expected_identity_pubkey": "02abcdef",
		"recover_state": true,
		"recovery_window": 144
	}`))
	require.NoError(t, err)
	defer clear(req.SeedEntropy)
	require.Equal(t, "/tmp/wavelength-wallet", cfg.DataDir)
	require.Equal(t, "regtest", cfg.Network)
	require.Equal(t, "btcwallet", cfg.WalletType)
	require.Equal(t, []byte{
		0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
		0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
	}, req.SeedEntropy)
	require.Equal(t, "02abcdef", req.ExpectedIdentityPubKey)
	require.True(t, req.RecoverState)
	require.Equal(t, uint32(144), req.RecoveryWindow)
}

// TestParseExternalSeedWalletStartRequestRejectsInvalid tests its wire shape.
func TestParseExternalSeedWalletStartRequestRejectsInvalid(t *testing.T) {
	tests := []struct{ name, request string }{
		{"flat config", `{
			"data_dir":"/flat",
			"seed_entropy":"ABEiM0RVZneImaq7zN3u/w=="
		}`},
		{"non-base64 entropy", `{
			"config":{"data_dir":"/tmp/wavelength-wallet"},
			"seed_entropy":"not_base64"
		}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := parseExternalSeedWalletStartRequest(
				[]byte(test.request),
			)
			require.Error(t, err)
		})
	}
}

// TestStartExternalSeedWalletRejectsBadRequestAndResets tests lifecycle reset.
func TestStartExternalSeedWalletRejectsBadRequestAndResets(t *testing.T) {
	resetMobileState(t)
	_, err := StartExternalSeedWallet([]byte(`{
		"config":{"data_dir":"/tmp/external-seed-test"},
		"seed_entropy":"AQ=="
	}`))
	require.Error(t, err)
	require.False(t, IsRunning())
}

// TestStartEmbeddedSeparatesLifecycleContexts tests context ownership.
func TestStartEmbeddedSeparatesLifecycleContexts(t *testing.T) {
	resetMobileState(t)

	var startCtx, operationCtx context.Context
	err := startEmbedded(func(start, operation context.Context) (
		*wavewalletdk.Client, error) {

		startCtx, operationCtx = start, operation

		return &wavewalletdk.Client{}, nil
	})
	require.NoError(t, err)
	_, startHasDeadline := startCtx.Deadline()
	_, operationHasDeadline := operationCtx.Deadline()
	require.True(t, startHasDeadline)
	require.False(t, operationHasDeadline)
	requireContextDone(t, startCtx, true)
	requireContextDone(t, operationCtx, false)

	_, activeCtx, err := activeClient()
	require.NoError(t, err)
	require.True(t, activeCtx == operationCtx)
	require.NoError(t, Stop())
	requireContextDone(t, operationCtx, true)
}

// TestStopCancelsStartupContexts tests cancellation during startup.
func TestStopCancelsStartupContexts(t *testing.T) {
	resetMobileState(t)

	type startupContexts struct {
		start, operation context.Context
	}
	returned := make(chan error, 1)
	published := make(chan startupContexts, 1)
	go func() {
		returned <- startEmbedded(
			func(start, operation context.Context) (
				*wavewalletdk.Client, error) {

				published <- startupContexts{start, operation}
				<-operation.Done()

				return nil, operation.Err()
			},
		)
	}()

	var contexts startupContexts
	select {
	case contexts = <-published:
	case <-t.Context().Done():
		t.Fatal("startup callback was not entered")
	}
	require.NoError(t, Stop())
	select {
	case err := <-returned:
		require.ErrorIs(t, err, context.Canceled)

	case <-t.Context().Done():
		t.Fatal("cancelled startup did not return")
	}
	requireContextDone(t, contexts.start, true)
	requireContextDone(t, contexts.operation, true)
	require.False(t, IsRunning())
}
