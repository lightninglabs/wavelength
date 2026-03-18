package swaps

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

type testInSwapServerConn struct {
	cfg *InSwapConfig
}

// RequestChannelID is unused in these tests.
func (c *testInSwapServerConn) RequestChannelID(
	_ context.Context, _ *btcec.PublicKey,
	_ uint32) (*RouteHint, *VHTLCConfig, error) {

	return nil, nil, nil
}

// CreateInSwap returns the preconfigured in-swap config.
func (c *testInSwapServerConn) CreateInSwap(
	context.Context, string, uint64,
	*btcec.PublicKey) (*InSwapConfig, error) {

	return c.cfg, nil
}

// Close closes the server connection.
func (c *testInSwapServerConn) Close() error {
	return nil
}

// TestPayViaLightningReturnsClaimPreimage asserts the SDK recovers the
// preimage from the spending OOR package after the vHTLC is claimed.
func TestPayViaLightningReturnsClaimPreimage(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			FeeSat:       123,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       144,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		sendSessionID: "funding-session",
		spentPreimage: &preimage,
	}

	client := NewSwapClient(serverConn, daemonConn, nil, nil)
	client.waitPollInterval = time.Millisecond

	result, err := client.PayViaLightning(
		t.Context(), "lnrtest1invoice", 0,
	)
	require.NoError(t, err)
	require.Equal(t, preimage.Hash(), result.PaymentHash)
	require.Equal(t, preimage, result.Preimage)
	require.Equal(t, "funding-session", result.FundingSessionID)
	require.EqualValues(t, 123, result.FeeSat)
}
