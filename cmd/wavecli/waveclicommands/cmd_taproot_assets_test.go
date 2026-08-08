package waveclicommands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestTaprootAssetOnboardingRequest verifies the friendly command preserves
// the proof bytes and maps every durable retry field to the daemon request.
func TestTaprootAssetOnboardingRequest(t *testing.T) {
	t.Parallel()

	proof := []byte{0, 1, 2, 3, 0xff}
	proofPath := filepath.Join(t.TempDir(), "asset-proof.tasset")
	require.NoError(t, os.WriteFile(proofPath, proof, 0o600))

	cmd := newTaprootAssetOnboardCmd()
	require.NoError(t, cmd.Flags().Set("idempotency-key", "deposit-1"))
	require.NoError(t, cmd.Flags().Set("asset-ref", "asset-id"))
	require.NoError(t, cmd.Flags().Set("asset-amount", "42"))
	require.NoError(t, cmd.Flags().Set("proof-file", proofPath))
	require.NoError(t, cmd.Flags().Set("carrier-value-sat", "1000"))
	require.NoError(t, cmd.Flags().Set("sat-per-vbyte", "2"))
	require.NoError(t, cmd.Flags().Set("max-fee-sat", "500"))

	request, err := taprootAssetOnboardingRequest(cmd)
	require.NoError(t, err)
	require.Equal(t, "deposit-1", request.IdempotencyKey)
	require.Equal(t, "asset-id", request.AssetRef)
	require.Equal(t, uint64(42), request.AssetAmount)
	require.Equal(t, proof, request.InputProofFile)
	require.Equal(t, uint64(1_000), request.CarrierValueSat)
	require.Equal(t, uint64(2), request.FeeRateSatPerVbyte)
	require.Zero(t, request.TargetConf)
	require.Equal(t, uint64(500), request.MaxFeeSat)
}

// TestTaprootAssetOnboardingRequestTargetConf verifies the estimator mode and
// the daemon-side operator-minimum carrier default remain explicit on the
// wire.
func TestTaprootAssetOnboardingRequestTargetConf(t *testing.T) {
	t.Parallel()

	proofPath := filepath.Join(t.TempDir(), "asset-proof.tasset")
	require.NoError(t, os.WriteFile(proofPath, []byte("proof"), 0o600))
	cmd := validTaprootAssetOnboardCmd(t, proofPath)
	require.NoError(t, cmd.Flags().Set("sat-per-vbyte", "0"))
	require.NoError(t, cmd.Flags().Set("target-conf", "6"))

	request, err := taprootAssetOnboardingRequest(cmd)
	require.NoError(t, err)
	require.Zero(t, request.CarrierValueSat)
	require.Zero(t, request.FeeRateSatPerVbyte)
	require.Equal(t, uint32(6), request.TargetConf)
}

func TestTaprootAssetOnboardingRequestRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	proofPath := filepath.Join(t.TempDir(), "asset-proof.tasset")
	require.NoError(t, os.WriteFile(proofPath, []byte("proof"), 0o600))

	tests := []struct {
		name    string
		flag    string
		value   string
		wantErr string
	}{
		{
			name:    "idempotency key",
			flag:    "idempotency-key",
			wantErr: "--idempotency-key is required",
		},
		{
			name:    "asset reference",
			flag:    "asset-ref",
			wantErr: "--asset-ref is required",
		},
		{
			name:    "asset amount",
			flag:    "asset-amount",
			value:   "0",
			wantErr: "--asset-amount must be positive",
		},
		{
			name:    "proof path",
			flag:    "proof-file",
			wantErr: "--proof-file is required",
		},
		{
			name:    "fee",
			flag:    "max-fee-sat",
			value:   "0",
			wantErr: "--max-fee-sat must be positive",
		},
		{
			name:  "fee selector",
			flag:  "sat-per-vbyte",
			value: "0",
			wantErr: "exactly one of --sat-per-vbyte and " +
				"--target-conf is required",
		},
		{
			name:    "two fee selectors",
			flag:    "target-conf",
			value:   "6",
			wantErr: "mutually exclusive",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cmd := validTaprootAssetOnboardCmd(t, proofPath)
			require.NoError(
				t, cmd.Flags().Set(test.flag, test.value),
			)
			_, err := taprootAssetOnboardingRequest(cmd)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func validTaprootAssetOnboardCmd(t *testing.T,
	proofPath string) *cobra.Command {

	t.Helper()

	cmd := newTaprootAssetOnboardCmd()
	require.NoError(t, cmd.Flags().Set("idempotency-key", "deposit-1"))
	require.NoError(t, cmd.Flags().Set("asset-ref", "asset-id"))
	require.NoError(t, cmd.Flags().Set("asset-amount", "42"))
	require.NoError(t, cmd.Flags().Set("proof-file", proofPath))
	require.NoError(t, cmd.Flags().Set("sat-per-vbyte", "2"))
	require.NoError(t, cmd.Flags().Set("max-fee-sat", "500"))

	return cmd
}
