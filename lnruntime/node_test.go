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
	)
	require.NoError(t, err)
	require.Equal(t, uint32(716), lockTime)
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
				testCase.height, testCase.terms,
			)
			require.Error(t, err)
		})
	}
}
