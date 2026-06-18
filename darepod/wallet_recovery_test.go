package darepod

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/vhtlcrecovery"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRetryRecoveryIndexerRPCRetriesResourceExhausted verifies seed recovery
// backs off and retries when the operator query limiter rejects a scan request.
func TestRetryRecoveryIndexerRPCRetriesResourceExhausted(t *testing.T) {
	t.Parallel()

	var attempts int
	err := retryRecoveryIndexerRPC(t.Context(), func() error {
		attempts++
		if attempts == 1 {
			return status.Error(
				codes.ResourceExhausted, "rate limited",
			)
		}

		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, attempts)
}

// TestRetryRecoveryIndexerRPCStopsOnContextCancel verifies recovery does not
// spin forever if the restore RPC is cancelled during rate-limit backoff.
func TestRetryRecoveryIndexerRPCStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var attempts int
	err := retryRecoveryIndexerRPC(ctx, func() error {
		attempts++

		return status.Error(codes.ResourceExhausted, "rate limited")
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, attempts)
}

// TestRecoverableVHTLCManifestFiltersPaySender verifies v1 restore ignores
// receiver/out-swap manifests and only considers pay-side sender refunds.
func TestRecoverableVHTLCManifestFiltersPaySender(t *testing.T) {
	t.Parallel()

	manifest, _ := testRecoveryManifest(t)
	require.True(t, recoverableVHTLCManifest(manifest))

	manifest.Role = vhtlcrecovery.ManifestRoleReceiver
	require.False(t, recoverableVHTLCManifest(manifest))

	manifest.Role = vhtlcrecovery.ManifestRoleSender
	manifest.Direction = "receive"
	require.False(t, recoverableVHTLCManifest(manifest))
}

// TestValidateVHTLCManifestScript verifies manifest metadata must rebuild the
// same script that recovery will query on the indexer.
func TestValidateVHTLCManifestScript(t *testing.T) {
	t.Parallel()

	manifest, _ := testRecoveryManifest(t)
	pkScript, err := validateVHTLCManifestScript(manifest)
	require.NoError(t, err)
	require.Equal(t, manifest.PkScript, pkScript)

	manifest.PkScript[len(manifest.PkScript)-1] ^= 0x01
	require.ErrorContains(
		t, func() error {
			_, err := validateVHTLCManifestScript(manifest)

			return err
		}(),
		"does not match",
	)
}

// TestRecoveredVHTLCRequestIDMatchesSDKShape keeps restore idempotent with rows
// normally armed by the swap SDK.
func TestRecoveredVHTLCRequestIDMatchesSDKShape(t *testing.T) {
	t.Parallel()

	manifest, _ := testRecoveryManifest(t)
	require.Equal(
		t,
		"sdk-swaps:pay:0101010101010101010101010101010101010101010101010101010101010101:VHTLC_RECOVERY_ACTION_REFUND_WITHOUT_RECEIVER", //nolint:ll
		recoveredVHTLCRequestID(manifest),
	)
}

// testRecoveryManifest builds a self-consistent pay-side vHTLC manifest.
func testRecoveryManifest(t *testing.T) (
	vhtlcrecovery.RecoveryManifest, []byte) {

	t.Helper()

	sender, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	receiver, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	server, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var paymentHash lntypes.Hash
	for i := range paymentHash {
		paymentHash[i] = 1
	}

	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:                               sender.PubKey(),
		Receiver:                             receiver.PubKey(),
		Server:                               server.PubKey(),
		PreimageHash:                         paymentHash,
		RefundLocktime:                       144,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                24,
		UnilateralRefundWithoutReceiverDelay: 36,
	})
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	pkScriptCopy := append([]byte(nil), pkScript...)

	return vhtlcrecovery.RecoveryManifest{
		Role:        vhtlcrecovery.ManifestRoleSender,
		Direction:   vhtlcrecovery.ManifestDirectionPay,
		PaymentHash: append([]byte(nil), paymentHash[:]...),
		SenderPubkey: sender.PubKey().
			SerializeCompressed(),
		ReceiverPubkey: receiver.PubKey().
			SerializeCompressed(),
		ServerPubkey: server.PubKey().
			SerializeCompressed(),
		RefundLocktime:                       144,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                24,
		UnilateralRefundWithoutReceiverDelay: 36,
		PkScript:                             pkScriptCopy,
		AmountSat:                            42_000,
		SignerKeyFamily: int32(
			keychain.KeyFamilyNodeKey,
		),
		SignerKeyIndex: 0,
		StatusHint:     "unsent_in_swap",
	}, pkScript
}
