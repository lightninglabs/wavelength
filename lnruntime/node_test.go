package lnruntime

import (
	"testing"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestAcceptedInvoiceChannel verifies accepted and settled HTLCs must all
// identify one nonzero incoming channel.
func TestAcceptedInvoiceChannel(t *testing.T) {
	t.Parallel()

	channelID := lnwire.NewShortChanIDFromInt(42)
	invoice := &invoices.Invoice{
		Htlcs: map[models.CircuitKey]*invoices.InvoiceHTLC{
			{
				ChanID: channelID,
				HtlcID: 1,
			}: {
				State: invoices.HtlcStateAccepted,
			},
			{
				ChanID: channelID,
				HtlcID: 2,
			}: {
				State: invoices.HtlcStateSettled,
			},
			{
				ChanID: channelID,
				HtlcID: 3,
			}: {
				State: invoices.HtlcStateCanceled,
			},
		},
	}

	actual, err := acceptedInvoiceChannel(invoice)
	require.NoError(t, err)
	require.Equal(t, channelID, actual)
}

// TestAcceptedInvoiceChannelRejectsAmbiguousDelivery verifies invoice
// acceptance cannot be attributed to a missing or second channel.
func TestAcceptedInvoiceChannelRejectsAmbiguousDelivery(t *testing.T) {
	t.Parallel()

	localInvoice := &invoices.Invoice{
		Htlcs: map[models.CircuitKey]*invoices.InvoiceHTLC{
			{
				HtlcID: 1,
			}: {
				State: invoices.HtlcStateAccepted,
			},
		},
	}
	multipleChannelInvoice := &invoices.Invoice{
		Htlcs: map[models.CircuitKey]*invoices.InvoiceHTLC{
			{
				ChanID: lnwire.NewShortChanIDFromInt(1),
				HtlcID: 1,
			}: {
				State: invoices.HtlcStateAccepted,
			},
			{
				ChanID: lnwire.NewShortChanIDFromInt(2),
				HtlcID: 2,
			}: {
				State: invoices.HtlcStateAccepted,
			},
		},
	}
	testCases := []struct {
		name    string
		invoice *invoices.Invoice
	}{
		{
			name: "nil invoice",
		},
		{
			name:    "no accepted htlc",
			invoice: &invoices.Invoice{},
		},
		{
			name:    "local htlc",
			invoice: localInvoice,
		},
		{
			name:    "multiple channels",
			invoice: multipleChannelInvoice,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := acceptedInvoiceChannel(testCase.invoice)
			require.Error(t, err)
		})
	}
}

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
