package swaps

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/stretchr/testify/require"
)

// TestAmountlessQuoteBinding rejects a server-proposed amount that differs
// from the payer's choice before any vHTLC can be funded.
func TestAmountlessQuoteBinding(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testAmountlessPayInvoice(t, preimage)
	cfg := testInSwapConfig(
		key.PubKey(), preimage, time.Now().Add(time.Minute),
	)
	require.NoError(
		t, validateInSwapQuote(
			invoice, testInSwapFeeSat, 0, cfg,
			&chaincfg.RegressionNetParams, testInSwapInvoiceSat,
		),
	)
	require.ErrorContains(
		t, validateInSwapQuote(
			invoice, testInSwapFeeSat, 0, cfg,
			&chaincfg.RegressionNetParams, testInSwapInvoiceSat+1,
		),
		"does not equal",
	)
	quote := &InSwapQuote{
		PaymentHash:      preimage.Hash(),
		InvoiceAmountSat: testInSwapInvoiceSat,
		AmountSat:        testInSwapAmountSat,
		FeeSat:           testInSwapFeeSat,
		SettlementType:   SettlementTypeLightning,
	}
	require.NoError(
		t, validateInSwapPreview(
			invoice, quote, 0, &chaincfg.RegressionNetParams,
			testInSwapInvoiceSat,
		),
	)
	require.ErrorContains(
		t, validateInSwapPreview(
			invoice, quote, 0, &chaincfg.RegressionNetParams,
			testInSwapInvoiceSat+1,
		),
		"does not match",
	)
}

// TestAmountlessAuthorizationBindsRetries proves that a lost admission response
// cannot be retried with a new amount, even through another SDK client
// instance.
func TestAmountlessAuthorizationBindsRetries(t *testing.T) {
	store := newTestSwapStore(t)
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testAmountlessPayInvoice(t, preimage)
	newClient := func() *SwapClient {
		return configureTestPayClient(
			NewSwapClientWithStore(nil, nil, nil, nil, store),
		)
	}
	client := newClient()
	_, err = client.authorizePay(
		t.Context(), invoice, 123, 0, key.PubKey(), 25000,
	)
	require.NoError(t, err)
	replay := newClient()
	_, err = replay.authorizePay(
		t.Context(), invoice, 123, 0, key.PubKey(), 25000,
	)
	require.NoError(t, err)
	_, err = replay.authorizePay(
		t.Context(), invoice, 123, 0, key.PubKey(), 25001,
	)
	require.ErrorIs(t, err, ErrPayAuthorizationMismatch)
	hash := preimage.Hash()
	auth, err := store.queries.GetPayAuthorization(t.Context(), hash[:])
	require.NoError(t, err)
	require.Equal(t, "25000", auth.AmountSat)
}

// CreateInSwapWithAmount records the payer principal at the transport boundary.
func (c *testInSwapServerConn) CreateInSwapWithAmount(ctx context.Context,
	invoice string, maxFeeSat uint64, key *btcec.PublicKey, account []byte,
	maxCreditSat uint64, proof *swaprpc.SwapOwnerProof, amountSat uint64) (
	*InSwapConfig, error) {

	c.payerAmountSat = amountSat

	return c.CreateInSwapWithCredits(
		ctx, invoice, maxFeeSat, key, account, maxCreditSat, proof,
	)
}

// TestAmountlessStartAndReplay binds server admission and persisted session
// reconstruction to the chosen amount without dispatching any Ark funds.
func TestAmountlessStartAndReplay(t *testing.T) {
	store := newTestSwapStore(t)
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testAmountlessPayInvoice(t, preimage)
	server := &testInSwapServerConn{
		cfg: testInSwapConfig(
			serverKey.PubKey(), preimage,
			time.Now().Add(time.Minute),
		),
	}
	daemon := &testDaemonConn{
		identityKey: key.PubKey(),
		operatorKey: serverKey.PubKey(),
	}
	newClient := func() *SwapClient {
		return configureTestPayClient(
			NewSwapClientWithStore(server, daemon, nil, nil, store),
		)
	}
	client := newClient()
	session, err := client.StartPayViaLightningWithAmount(
		t.Context(), invoice, testInSwapFeeSat, 0, testInSwapInvoiceSat,
	)
	require.NoError(t, err)
	require.Equal(t, uint64(testInSwapInvoiceSat), server.payerAmountSat)
	require.Equal(t, PayStateSwapCreated, session.State())
	replay := newClient()
	resumed, err := replay.StartPayViaLightningWithAmount(
		t.Context(), invoice, testInSwapFeeSat, 0, testInSwapInvoiceSat,
	)
	require.NoError(t, err)
	require.Equal(t, uint64(testInSwapInvoiceSat), resumed.amountSat)
	_, err = replay.StartPayViaLightningWithAmount(
		t.Context(), invoice, testInSwapFeeSat, 0,
		testInSwapInvoiceSat+1,
	)
	require.ErrorIs(t, err, ErrPayAuthorizationMismatch)
	require.Equal(t, 1, server.createCalls)
	require.Zero(t, daemon.sendPolicyCalls)
}
