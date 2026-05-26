package swaps

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

// TestValidateRouteHintRejectsTruncatedFields verifies route-hint fields are
// range checked before they are cast into the narrower zpay32 hop hint shape.
func TestValidateRouteHintRejectsTruncatedFields(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nodeID := privKey.PubKey().SerializeCompressed()

	routeHint := &RouteHint{
		NodeID:      nodeID,
		FeeBaseMsat: uint64(^uint32(0)) + 1,
	}
	err = validateRouteHint(routeHint)
	require.ErrorContains(t, err, "fee base msat")

	routeHint = &RouteHint{
		NodeID:     nodeID,
		FeePropPpm: uint64(^uint32(0)) + 1,
	}
	err = validateRouteHint(routeHint)
	require.ErrorContains(t, err, "fee proportional ppm")

	routeHint = &RouteHint{
		NodeID:          nodeID,
		CltvExpiryDelta: uint32(^uint16(0)) + 1,
	}
	err = validateRouteHint(routeHint)
	require.ErrorContains(t, err, "CLTV expiry delta")
}

// TestInvoiceGeneratorIncludesPaymentAddress verifies generated invoices are
// accepted by modern LND senders, which reject BOLT11 invoices without either a
// payment address or blinded paths.
func TestInvoiceGeneratorIncludesPaymentAddress(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	creator := NewEphemeralInvoiceGenerator(
		privKey, nil, &chaincfg.RegressionNetParams,
	)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	invoice, hash, err := creator.CreateInvoice(
		context.Background(), btcutil.Amount(50_000), "swap",
		&RouteHint{
			NodeID:          privKey.PubKey().SerializeCompressed(),
			ChannelID:       42,
			CltvExpiryDelta: 40,
		},
		time.Hour, &preimage,
	)
	require.NoError(t, err)
	require.Equal(t, preimage.Hash(), hash)
	require.Equal(t, preimage, *invoice.Terms.PaymentPreimage)
	expectedMSat := lnwire.NewMSatFromSatoshis(50_000)
	require.Equal(t, expectedMSat, invoice.Terms.Value)
	require.NotZero(t, invoice.Terms.PaymentAddr)
	require.Equal(t, time.Hour, invoice.Terms.Expiry)
	require.True(
		t, invoice.Terms.Features.HasFeature(
			lnwire.TLVOnionPayloadOptional,
		),
	)
	require.True(
		t, invoice.Terms.Features.HasFeature(
			lnwire.PaymentAddrOptional,
		),
	)

	decoded, err := zpay32.Decode(
		string(invoice.PaymentRequest), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.True(t, decoded.PaymentAddr.IsSome())
	decodedPayAddr := decoded.PaymentAddr.UnwrapOr([32]byte{})
	require.Equal(t, invoice.Terms.PaymentAddr, decodedPayAddr)
	require.True(
		t, decoded.Features.HasFeature(
			lnwire.TLVOnionPayloadOptional,
		),
	)
	require.True(t, decoded.Features.HasFeature(lnwire.PaymentAddrOptional))
}

// TestInvoiceGeneratorPreservesPayerFeeRouteHint verifies receive invoices
// encode the payer-paid route fee and keep multi-part payments disabled.
func TestInvoiceGeneratorPreservesPayerFeeRouteHint(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	creator := NewEphemeralInvoiceGenerator(
		privKey, nil, &chaincfg.RegressionNetParams,
	)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	routeHint := &RouteHint{
		NodeID:          privKey.PubKey().SerializeCompressed(),
		ChannelID:       42,
		FeeBaseMsat:     0,
		FeePropPpm:      10_000,
		CltvExpiryDelta: 40,
	}

	invoice, _, err := creator.CreateInvoice(
		context.Background(), btcutil.Amount(50_000),
		"swap", routeHint, time.Hour, &preimage,
	)
	require.NoError(t, err)

	decoded, err := zpay32.Decode(
		string(invoice.PaymentRequest), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.Len(t, decoded.RouteHints, 1)
	require.Len(t, decoded.RouteHints[0], 1)

	hop := decoded.RouteHints[0][0]
	require.Equal(t, uint32(0), hop.FeeBaseMSat)
	require.Equal(t, uint32(10_000), hop.FeeProportionalMillionths)
	require.False(t, decoded.Features.HasFeature(lnwire.MPPOptional))
	require.False(t, decoded.Features.HasFeature(lnwire.MPPRequired))
}
