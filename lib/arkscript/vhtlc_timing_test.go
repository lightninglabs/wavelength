package arkscript

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestVHTLCTimingValidateOrder verifies that less cooperative refund paths
// cannot mature before the receiver's claim path.
func TestVHTLCTimingValidateOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		timing  VHTLCTiming
		wantErr string
	}{
		{
			name: "ordered",
			timing: VHTLCTiming{
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
		},
		{
			name: "equal delays",
			timing: VHTLCTiming{
				UnilateralClaimDelay:                 144,
				UnilateralRefundDelay:                144,
				UnilateralRefundWithoutReceiverDelay: 144,
			},
		},
		{
			name: "claim after cooperative refund",
			timing: VHTLCTiming{
				UnilateralClaimDelay:                 25,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			wantErr: "claim delay 25 exceeds",
		},
		{
			name: "cooperative refund after sender refund",
			timing: VHTLCTiming{
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                37,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			wantErr: "refund delay 37 exceeds",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := test.timing.ValidateOrder()
			if test.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

// TestVHTLCTimingValidateClaimWindow verifies strict refund-window arithmetic
// at its exact boundary.
func TestVHTLCTimingValidateClaimWindow(t *testing.T) {
	t.Parallel()

	timing := VHTLCTiming{
		RefundLocktime:                       329,
		UnilateralClaimDelay:                 144,
		UnilateralRefundDelay:                144,
		UnilateralRefundWithoutReceiverDelay: 256,
	}
	window := VHTLCClaimWindow{
		CurrentHeight:     100,
		ExitAncestryDelay: 72,
		RecoveryMargin:    12,
	}

	require.NoError(t, timing.ValidateClaimWindow(window))
	timing.RefundLocktime = 328
	err := timing.ValidateClaimWindow(window)
	require.ErrorContains(
		t, err, "refund window 228 blocks does not exceed required "+
			"claim window 228",
	)

	timing.RefundLocktime = 100
	err = timing.ValidateClaimWindow(window)
	require.ErrorContains(t, err, "is not after current height")
}

// TestDecodeVHTLCTiming verifies that encoded semantic policies retain the
// timing tuple and that lookalike policies with mismatched participants fail.
func TestDecodeVHTLCTiming(t *testing.T) {
	t.Parallel()

	opts := testVHTLCOpts(t)
	policy, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	encoded, err := policy.Template.Encode()
	require.NoError(t, err)
	decoded, err := DecodePolicyTemplate(encoded)
	require.NoError(t, err)

	timing, err := DecodeVHTLCTiming(decoded)
	require.NoError(t, err)
	require.Equal(t, opts.Timing(), *timing)

	bad, err := DecodePolicyTemplate(encoded)
	require.NoError(t, err)
	for i := range bad.Leaves {
		shape, shapeErr := decodeVHTLCNode(bad.Leaves[i].Node)
		require.NoError(t, shapeErr)
		if shape.csvDelay > 0 && shape.locktime == 0 &&
			len(shape.predicate) == 0 && len(shape.keys) == 2 {

			csv, ok := bad.Leaves[i].Node.(*CSV)
			require.True(t, ok)
			multisig, ok := csv.Inner.(*Multisig)
			require.True(t, ok)
			multisig.Keys[0] = opts.Server

			break
		}
	}

	_, err = DecodeVHTLCTiming(bad)
	require.ErrorContains(t, err, "unilateral refund keys do not match")

	badCSV, err := DecodePolicyTemplate(encoded)
	require.NoError(t, err)
	for i := range badCSV.Leaves {
		csv, ok := badCSV.Leaves[i].Node.(*CSV)
		if !ok {
			continue
		}

		csv.Lock |= wire.SequenceLockTimeIsSeconds

		break
	}

	_, err = DecodeVHTLCTiming(badCSV)
	require.ErrorContains(t, err, "canonical block delay")

	unorderedOpts := opts
	unorderedOpts.UnilateralClaimDelay =
		unorderedOpts.UnilateralRefundDelay + 1
	unorderedPolicy, err := NewVHTLCPolicy(unorderedOpts)
	require.NoError(t, err)
	unorderedEncoded, err := unorderedPolicy.Template.Encode()
	require.NoError(t, err)
	unordered, err := DecodePolicyTemplate(unorderedEncoded)
	require.NoError(t, err)

	unorderedTiming, err := DecodeVHTLCTiming(unordered)
	require.NoError(t, err)
	require.Equal(t, unorderedOpts.Timing(), *unorderedTiming)
}
