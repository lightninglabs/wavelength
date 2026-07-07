package swaps

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightningnetwork/lnd/keychain"
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
		NodeID:          nodeID,
		ChannelID:       42,
		FeeBaseMsat:     uint64(^uint32(0)) + 1,
		CltvExpiryDelta: 40,
	}
	err = validateRouteHint(routeHint)
	require.ErrorContains(t, err, "fee base msat")

	routeHint = &RouteHint{
		NodeID:          nodeID,
		ChannelID:       42,
		FeePropPpm:      uint64(^uint32(0)) + 1,
		CltvExpiryDelta: 40,
	}
	err = validateRouteHint(routeHint)
	require.ErrorContains(t, err, "fee proportional ppm")

	routeHint = &RouteHint{
		NodeID:          nodeID,
		ChannelID:       42,
		CltvExpiryDelta: uint32(^uint16(0)) + 1,
	}
	err = validateRouteHint(routeHint)
	require.ErrorContains(t, err, "CLTV expiry delta")
}

// TestValidateRouteHintRejectsZeroFields verifies unusable zero-valued hop
// fields are rejected before invoice creation.
func TestValidateRouteHintRejectsZeroFields(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nodeID := privKey.PubKey().SerializeCompressed()

	routeHint := &RouteHint{
		NodeID:          nodeID,
		CltvExpiryDelta: 40,
	}
	err = validateRouteHint(routeHint)
	require.ErrorContains(t, err, "channel ID")

	routeHint = &RouteHint{
		NodeID:    nodeID,
		ChannelID: 42,
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
// encode the payer-paid route fee and advertise optional multi-part
// payments.
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
	require.True(t, decoded.Features.IsSet(lnwire.MPPOptional))
	require.False(t, decoded.Features.IsSet(lnwire.MPPRequired))
}

// TestInvoiceGeneratorEmbedsRouteHintPath verifies receive invoices encode a
// private ingress hop before the swap server's virtual hop.
func TestInvoiceGeneratorEmbedsRouteHintPath(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	gatewayKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	creator := NewEphemeralInvoiceGenerator(
		privKey, nil, &chaincfg.RegressionNetParams,
	)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	routeHintPath := []*RouteHint{{
		NodeID:          gatewayKey.PubKey().SerializeCompressed(),
		ChannelID:       21,
		FeeBaseMsat:     1,
		FeePropPpm:      2,
		CltvExpiryDelta: 40,
	}, {
		NodeID:          privKey.PubKey().SerializeCompressed(),
		ChannelID:       42,
		FeeBaseMsat:     0,
		FeePropPpm:      10_000,
		CltvExpiryDelta: 60,
	}}

	authKey := keychain.NewPrivKeyMessageSigner(
		privKey, keychain.KeyLocator{},
	)
	invoice, _, err := creator.CreateInvoiceWithKeyRouteHintPaths(
		context.Background(), btcutil.Amount(50_000),
		"swap", [][]*RouteHint{routeHintPath}, time.Hour, authKey,
		&preimage,
	)
	require.NoError(t, err)

	decoded, err := zpay32.Decode(
		string(invoice.PaymentRequest), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.Len(t, decoded.RouteHints, 1)
	require.Len(t, decoded.RouteHints[0], 2)

	require.Equal(t, uint64(21), decoded.RouteHints[0][0].ChannelID)
	require.Equal(t, uint32(1), decoded.RouteHints[0][0].FeeBaseMSat)
	require.Equal(
		t, uint32(2),
		decoded.RouteHints[0][0].FeeProportionalMillionths,
	)
	require.Equal(
		t, uint16(40), decoded.RouteHints[0][0].CLTVExpiryDelta,
	)
	require.Equal(t, uint64(42), decoded.RouteHints[0][1].ChannelID)
	require.Equal(
		t, uint32(10_000),
		decoded.RouteHints[0][1].FeeProportionalMillionths,
	)
	require.Equal(
		t, uint16(60), decoded.RouteHints[0][1].CLTVExpiryDelta,
	)
}

// TestInvoiceGeneratorEmbedsMultipleRouteHintPaths verifies that a receive
// invoice carries one BOLT-11 "r" field per alternative route-hint path, all
// terminating at the same virtual channel. This is the multi-backend fan-out
// shape: each path enters through a different backend node.
func TestInvoiceGeneratorEmbedsMultipleRouteHintPaths(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	backendOneKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	backendTwoKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	creator := NewEphemeralInvoiceGenerator(
		privKey, nil, &chaincfg.RegressionNetParams,
	)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	const virtualChannelID = 42
	routeHintPaths := [][]*RouteHint{{{
		NodeID:          backendOneKey.PubKey().SerializeCompressed(),
		ChannelID:       virtualChannelID,
		FeeBaseMsat:     0,
		FeePropPpm:      10_000,
		CltvExpiryDelta: 60,
	}}, {{
		NodeID:          backendTwoKey.PubKey().SerializeCompressed(),
		ChannelID:       virtualChannelID,
		FeeBaseMsat:     0,
		FeePropPpm:      10_000,
		CltvExpiryDelta: 60,
	}}}

	authKey := keychain.NewPrivKeyMessageSigner(
		privKey, keychain.KeyLocator{},
	)
	invoice, _, err := creator.CreateInvoiceWithKeyRouteHintPaths(
		context.Background(), btcutil.Amount(50_000),
		"swap", routeHintPaths, time.Hour, authKey, &preimage,
	)
	require.NoError(t, err)

	decoded, err := zpay32.Decode(
		string(invoice.PaymentRequest), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.Len(t, decoded.RouteHints, 2)

	for i, hint := range decoded.RouteHints {
		require.Len(t, hint, 1, "path %d", i)
		require.Equal(
			t, uint64(virtualChannelID), hint[0].ChannelID, "pat"+
				"h %d", i,
		)
		require.Equal(
			t, uint32(10_000), hint[0].FeeProportionalMillionths,
			"path %d", i,
		)
	}
	require.Equal(
		t, backendOneKey.PubKey().SerializeCompressed(),
		decoded.RouteHints[0][0].NodeID.SerializeCompressed(),
	)
	require.Equal(
		t, backendTwoKey.PubKey().SerializeCompressed(),
		decoded.RouteHints[1][0].NodeID.SerializeCompressed(),
	)
}

// TestInvoiceGeneratorRejectsTooManyRouteHintPaths verifies a quote carrying
// more paths than lnd accepts as hop hints fails fast with the path count.
func TestInvoiceGeneratorRejectsTooManyRouteHintPaths(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	backendKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	creator := NewEphemeralInvoiceGenerator(
		privKey, nil, &chaincfg.RegressionNetParams,
	)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	routeHintPaths := make([][]*RouteHint, maxRouteHintPaths+1)
	for i := range routeHintPaths {
		routeHintPaths[i] = []*RouteHint{{
			NodeID: backendKey.
				PubKey().
				SerializeCompressed(),
			ChannelID:       42,
			FeeBaseMsat:     0,
			FeePropPpm:      10_000,
			CltvExpiryDelta: 60,
		}}
	}

	authKey := keychain.NewPrivKeyMessageSigner(
		privKey, keychain.KeyLocator{},
	)
	_, _, err = creator.CreateInvoiceWithKeyRouteHintPaths(
		context.Background(), btcutil.Amount(50_000),
		"swap", routeHintPaths, time.Hour, authKey, &preimage,
	)
	require.ErrorContains(t, err, "21 route hint paths exceed the maximum")
}
