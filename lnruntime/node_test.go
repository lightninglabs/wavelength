package lnruntime

import (
	"testing"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/stretchr/testify/require"
)

// TestArkChannelPaymentLockTime proves an unmaterialized channel reserves the
// complete source-recovery horizon before the ordinary Lightning CLTV margin.
func TestArkChannelPaymentLockTime(t *testing.T) {
	lockTime, err := arkChannelPaymentLockTime(
		100, arkchannel.VTXOTerms{
			ChannelDelay: 144,
			FunderDelay:  576,
		},
		0,
	)
	require.NoError(t, err)
	require.Equal(t, uint32(716), lockTime)
}

// TestArkChannelPaymentLockTimeEnforcesMaximum proves an exact boundary is
// accepted while a private HTLC that would outlive it is rejected before
// dispatch.
func TestArkChannelPaymentLockTimeEnforcesMaximum(t *testing.T) {
	t.Parallel()

	terms := arkchannel.VTXOTerms{
		ChannelDelay: 144,
		FunderDelay:  576,
	}
	lockTime, err := arkChannelPaymentLockTime(100, terms, 716)
	require.NoError(t, err)
	require.Equal(t, uint32(716), lockTime)

	_, err = arkChannelPaymentLockTime(100, terms, 715)
	require.ErrorIs(t, err, ErrPrivatePaymentExpiryNotNested)
}

// TestMaxNestedPrivateCLTVExpiry proves the private expiry leaves the complete
// public settlement margin plus one strict boundary block.
func TestMaxNestedPrivateCLTVExpiry(t *testing.T) {
	t.Parallel()

	maxExpiry, err := MaxNestedPrivateCLTVExpiry(722, 5)
	require.NoError(t, err)
	require.Equal(t, uint32(716), maxExpiry)
	require.Less(t, maxExpiry+5, uint32(722))

	_, err = MaxNestedPrivateCLTVExpiry(6, 5)
	require.ErrorIs(t, err, ErrPrivatePaymentExpiryNotNested)
	_, err = MaxNestedPrivateCLTVExpiry(5, ^uint32(0))
	require.ErrorIs(t, err, ErrPrivatePaymentExpiryNotNested)
}

// TestArkChannelPaymentLockTimeRejectsInvalidTerms proves malformed delays and
// arithmetic overflow cannot silently shorten the recovery window.
func TestArkChannelPaymentLockTimeRejectsInvalidTerms(t *testing.T) {
	testCases := []struct {
		name   string
		height uint32
		terms  arkchannel.VTXOTerms
	}{
		{
			name: "funder before channel",
			terms: arkchannel.VTXOTerms{
				ChannelDelay: 144,
				FunderDelay:  143,
			},
		},
		{
			name: "delta overflow",
			terms: arkchannel.VTXOTerms{
				ChannelDelay: 1,
				FunderDelay:  maximumBlockHeight,
			},
		},
		{
			name:   "height overflow",
			height: maximumBlockHeight - 10,
			terms: arkchannel.VTXOTerms{
				ChannelDelay: 1,
				FunderDelay:  1,
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := arkChannelPaymentLockTime(
				testCase.height, testCase.terms, 0,
			)
			require.Error(t, err)
		})
	}
}

// TestArkChannelPaymentCLTVDeltaRejectsLinkLimit proves channel policy is
// rejected before invoice advertisement when lnd's link cannot carry the
// required private lifetime.
func TestArkChannelPaymentCLTVDeltaRejectsLinkLimit(t *testing.T) {
	t.Parallel()

	_, err := ArkChannelPaymentCLTVDelta(arkchannel.VTXOTerms{
		ChannelDelay: 1,
		FunderDelay:  2_000,
	})
	require.ErrorContains(t, err, "exceeds lnd maximum")
}
