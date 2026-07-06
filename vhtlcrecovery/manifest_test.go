package vhtlcrecovery

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecoveryManifestLabelRoundTrip(t *testing.T) {
	t.Parallel()

	manifest := RecoveryManifest{
		Role:                                 ManifestRoleSender,
		Direction:                            ManifestDirectionPay,
		PaymentHash:                          bytesOf(32, 1),
		SenderPubkey:                         bytesOf(33, 2),
		ReceiverPubkey:                       bytesOf(33, 3),
		ServerPubkey:                         bytesOf(33, 4),
		RefundLocktime:                       144,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                24,
		UnilateralRefundWithoutReceiverDelay: 36,
		PkScript:                             bytesOf(34, 5),
		AmountSat:                            42_000,
		SignerKeyFamily:                      6,
		SignerKeyIndex:                       0,
		StatusHint:                           "unsent_in_swap",
	}

	label, err := EncodeManifestLabel(manifest)
	require.NoError(t, err)
	require.Contains(t, label, ManifestLabelPrefix)

	decoded, ok, err := DecodeManifestLabel(label)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, manifest, decoded)
}

func TestDecodeManifestLabelIgnoresOtherLabels(t *testing.T) {
	t.Parallel()

	_, ok, err := DecodeManifestLabel("oor receive")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestDecodeManifestLabelRejectsCorruptPayload(t *testing.T) {
	t.Parallel()

	_, ok, err := DecodeManifestLabel(ManifestLabelPrefix + "not-base64!")
	require.Error(t, err)
	require.True(t, ok)
}

// bytesOf returns a deterministic byte slice for manifest round-trip tests.
func bytesOf(length int, value byte) []byte {
	out := make([]byte, length)
	for i := range out {
		out[i] = value
	}

	return out
}
