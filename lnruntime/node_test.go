package lnruntime

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestAcceptedInvoiceDeadlineReadsNativeRegistry exercises the real LND hold
// invoice acceptance and cancellation APIs against its channel DB. A second
// adapter sees current registry state rather than cached bridge acceptance.
func TestAcceptedInvoiceDeadlineReadsNativeRegistry(t *testing.T) {
	t.Parallel()
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	fixture := newTestRuntime(
		t, key,
		input.NewMockSigner(
			[]*btcec.PrivateKey{key}, &chaincfg.RegressionNetParams,
		),
	)
	require.NoError(t, fixture.runtime.Start())
	t.Cleanup(func() { require.NoError(t, fixture.runtime.Stop()) })
	node := &NativeNode{runtime: fixture.runtime}
	preimage := lntypes.Preimage{9}
	hash := preimage.Hash()
	require.NoError(t, node.AddHoldInvoice(t.Context(), hash, 2_000))
	channel := lnwire.NewShortChanIDFromInt(42)
	resolution, err := fixture.runtime.Invoices().NotifyExitHopHtlc(
		hash, 2_000_000, 801_000, 800_000,
		models.CircuitKey{ChanID: channel, HtlcID: 1},
		make(chan interface{}, 1), nil, &hop.Payload{},
	)
	require.NoError(t, err)
	require.Nil(t, resolution)
	require.NoError(t, node.WaitInvoiceAccepted(t.Context(), hash))
	terms := arkchannel.Terms{
		ReservedSCID: 42,
		VTXO: arkchannel.VTXOTerms{
			ChannelDelay: 144, FunderDelay: 576,
		},
	}
	record := arkchannel.Record{Snapshot: arkchannel.Snapshot{Terms: terms}}
	restored := &NativeNode{runtime: fixture.runtime}
	deadline, err := restored.AcceptedInvoiceDeadline(
		t.Context(), record, hash, 2_000,
	)
	require.NoError(t, err)
	require.Equal(t, uint32(800_381), deadline)
	require.NoError(t, node.CancelInvoice(t.Context(), hash))
	_, err = restored.AcceptedInvoiceDeadline(
		t.Context(), record, hash, 2_000,
	)
	var notStarted *PaymentNotStartedError
	require.ErrorAs(t, err, &notStarted)
	err = restored.WaitInvoiceAccepted(t.Context(), hash)
	require.ErrorAs(t, err, &notStarted)
}

// TestArkChannelReceiveCLTVDelta includes a second complete recovery horizon
// for outcome reconciliation before the public interceptor can auto-fail.
func TestArkChannelReceiveCLTVDelta(t *testing.T) {
	t.Parallel()
	terms := arkchannel.VTXOTerms{ChannelDelay: 144, FunderDelay: 576}
	delta, err := ArkChannelReceiveCLTVDelta(terms)
	require.NoError(t, err)
	require.Equal(t, uint32(1_272), delta)
	private, err := ArkChannelPaymentCLTVDelta(terms)
	require.NoError(t, err)
	privateExpiry, err := MaxNestedPrivateCLTVExpiry(
		100+delta+18-19, private+5,
	)
	require.NoError(t, err)
	require.LessOrEqual(t, 100+12+private, privateExpiry)
}

// TestAcceptedInvoiceExpiryUsesNativeHTLCs proves the earliest live shard,
// rather than invoice final-CLTV preferences or a canceled shard, binds a
// nested dispatch after restart.
func TestAcceptedInvoiceExpiryUsesNativeHTLCs(t *testing.T) {
	t.Parallel()
	channel := lnwire.NewShortChanIDFromInt(42)
	invoice := &invoices.Invoice{
		State: invoices.ContractAccepted, HodlInvoice: true,
		Terms: invoices.ContractTerm{
			Value: 2_000_000,
		},
		Htlcs: map[models.CircuitKey]*invoices.InvoiceHTLC{
			{
				ChanID: channel,
				HtlcID: 1,
			}: {
				State:  invoices.HtlcStateAccepted,
				Amt:    1_000_000,
				Expiry: 800,
			},
			{
				ChanID: channel,
				HtlcID: 2,
			}: {
				State:  invoices.HtlcStateAccepted,
				Amt:    1_000_000,
				Expiry: 700,
			},
			{
				ChanID: channel,
				HtlcID: 3,
			}: {
				State:  invoices.HtlcStateCanceled,
				Amt:    2_000_000,
				Expiry: 500,
			},
		},
	}
	expiry, err := acceptedInvoiceExpiry(invoice, channel, 2_000)
	require.NoError(t, err)
	require.Equal(t, uint32(700), expiry)

	// The native registry can cancel the invoice while the worker is down.
	invoice.State = invoices.ContractCanceled
	_, err = acceptedInvoiceExpiry(invoice, channel, 2_000)
	require.Error(t, err)
}

// TestAcceptedInvoiceExpiryRejectsUnfundedSources proves admission cannot use
// the wrong channel, incomplete value, or already-released collateral.
func TestAcceptedInvoiceExpiryRejectsUnfundedSources(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		channel    uint64
		amount     btcutil.Amount
		state      invoices.ContractState
		htlcAmount lnwire.MilliSatoshi
	}{
		{
			name: "wrong channel", channel: 43, amount: 2_000,
			state:      invoices.ContractAccepted,
			htlcAmount: 2_000_000,
		},
		{
			name: "wrong amount", channel: 42, amount: 3_000,
			state:      invoices.ContractAccepted,
			htlcAmount: 2_000_000,
		},
		{
			name: "incomplete amount", channel: 42, amount: 2_000,
			state:      invoices.ContractAccepted,
			htlcAmount: 1_000_000,
		},
		{
			name: "overpaid shard", channel: 42, amount: 2_000,
			state:      invoices.ContractAccepted,
			htlcAmount: 2_000_001,
		},
		{
			name: "preimage already released", channel: 42,
			amount: 2_000, state: invoices.ContractSettled,
			htlcAmount: 2_000_000,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			channel := lnwire.NewShortChanIDFromInt(42)
			key := models.CircuitKey{ChanID: channel}
			htlcs := make(
				map[models.CircuitKey]*invoices.InvoiceHTLC,
			)
			htlcs[key] = &invoices.InvoiceHTLC{
				State: invoices.HtlcStateAccepted,
				Amt:   test.htlcAmount, Expiry: 700,
			}
			invoice := &invoices.Invoice{
				State: test.state, HodlInvoice: true,
				Terms: invoices.ContractTerm{
					Value: 2_000_000,
				},
				Htlcs: htlcs,
			}
			_, err := acceptedInvoiceExpiry(
				invoice,
				lnwire.NewShortChanIDFromInt(test.channel),
				test.amount,
			)
			require.Error(t, err)
		})
	}
}

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
		0, arkchannel.PartyHub,
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
	lockTime, err := arkChannelPaymentLockTime(
		100, terms, 716, arkchannel.PartyHub,
	)
	require.NoError(t, err)
	require.Equal(t, uint32(716), lockTime)

	_, err = arkChannelPaymentLockTime(
		100, terms, 715, arkchannel.PartyHub,
	)
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
				arkchannel.PartyHub,
			)
			require.Error(t, err)
		})
	}
}

// TestOutgoingPrivateLifetimeFundsPublicLeg keeps enough lifetime for a
// public payment after reserving the complete source recovery allowance.
func TestOutgoingPrivateLifetimeFundsPublicLeg(t *testing.T) {
	t.Parallel()
	terms := arkchannel.VTXOTerms{ChannelDelay: 144, FunderDelay: 576}
	const height = uint32(100)
	expiry, err := arkChannelPaymentLockTime(
		height, terms, 0, arkchannel.PartyClient,
	)
	require.NoError(t, err)
	require.Equal(t, uint32(1332), expiry)
	reserve := terms.FunderDelay + arkChannelPaymentCLTVMarginBlocks +
		defaultOutgoingRejectDelta
	require.Greater(t, expiry-reserve, height+40)
	_, err = arkChannelPaymentLockTime(
		height, terms, expiry-1, arkchannel.PartyClient,
	)
	require.ErrorIs(t, err, ErrPrivatePaymentExpiryNotNested)
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
