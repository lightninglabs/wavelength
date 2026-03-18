package swaps

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

type testInvoiceCreator struct {
	invoice     *invoices.Invoice
	paymentHash lntypes.Hash
}

// CreateInvoice returns the preconfigured invoice and payment hash.
func (c *testInvoiceCreator) CreateInvoice(_ context.Context,
	_ btcutil.Amount, _ string, _ *RouteHint, _ time.Duration,
	_ *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash, error) {

	return c.invoice, c.paymentHash, nil
}

type testSwapServerConn struct {
	hint *RouteHint
	cfg  *VHTLCConfig
}

// RequestChannelID returns the preconfigured out-swap route hint.
func (c *testSwapServerConn) RequestChannelID(context.Context,
	*btcec.PublicKey, uint32) (*RouteHint, *VHTLCConfig, error) {

	return c.hint, c.cfg, nil
}

// CreateInSwap is unused in these tests.
func (c *testSwapServerConn) CreateInSwap(context.Context, string, uint64,
	*btcec.PublicKey) (*InSwapConfig, error) {

	return nil, nil
}

// Close closes the server connection.
func (c *testSwapServerConn) Close() error {
	return nil
}

type testDaemonConn struct {
	identityKey    *btcec.PublicKey
	operatorKey    *btcec.PublicKey
	vhtlc          *VTXOInfo
	receiveScript  []byte
	sendSessionID  string
	spentPreimage  *lntypes.Preimage
	lastClaimInput []CustomInput
}

// SendOOR is unused in these tests.
func (d *testDaemonConn) SendOOR(context.Context, []byte, int64) (string,
	error) {

	return d.sendSessionID, nil
}

// SendOORWithCustomInputs records the claim request.
func (d *testDaemonConn) SendOORWithCustomInputs(_ context.Context,
	_ []byte, _ int64, inputs []CustomInput) (string, error) {

	d.lastClaimInput = append([]CustomInput(nil), inputs...)

	return d.sendSessionID, nil
}

// GetIdentityPubkey returns the configured client key.
func (d *testDaemonConn) GetIdentityPubkey(context.Context) (
	*btcec.PublicKey, error) {

	return d.identityKey, nil
}

// GetOperatorPubkey returns the configured operator key.
func (d *testDaemonConn) GetOperatorPubkey(context.Context) (
	*btcec.PublicKey, error) {

	return d.operatorKey, nil
}

// ListLiveVTXOs is unused in these tests.
func (d *testDaemonConn) ListLiveVTXOs(context.Context) ([]VTXOInfo, error) {
	return nil, nil
}

// FindLiveVTXOByPkScript returns the preconfigured vHTLC.
func (d *testDaemonConn) FindLiveVTXOByPkScript(context.Context,
	[]byte) (*VTXOInfo, error) {

	return d.vhtlc, nil
}

// GetSpentVTXOPreimage is unused in these tests.
func (d *testDaemonConn) GetSpentVTXOPreimage(context.Context,
	lntypes.Hash, []byte) (*lntypes.Preimage, error) {

	return d.spentPreimage, nil
}

// NewOORReceiveScript returns the configured receive script.
func (d *testDaemonConn) NewOORReceiveScript(context.Context) ([]byte, error) {
	return append([]byte(nil), d.receiveScript...), nil
}

// TestReceiveSessionWaitClaimsVHTLC asserts the SDK owns the route-hint,
// invoice, wait, and claim-path logic for out-swaps.
func TestReceiveSessionWaitClaimsVHTLC(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	hash := lntypes.Hash(sha256.Sum256(preimage[:]))
	creator := &testInvoiceCreator{
		invoice: &invoices.Invoice{
			PaymentRequest: []byte("lnrtest1swap"),
		},
		paymentHash: hash,
	}

	serverConn := &testSwapServerConn{
		hint: &RouteHint{
			NodeID:          serverPriv.PubKey().SerializeCompressed(),
			ChannelID:       99,
			FeeBaseMsat:     1,
			FeePropPpm:      2,
			CltvExpiryDelta: 40,
		},
		cfg: &VHTLCConfig{
			RefundLocktime:                       144,
			UnilateralClaimDelay:                 12,
			UnilateralRefundDelay:                24,
			UnilateralRefundWithoutReceiverDelay: 36,
			SwapServerPubkey: serverPriv.PubKey().
				SerializeCompressed(),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		vhtlc:         &VTXOInfo{Outpoint: "txid:0", AmountSat: 42_000},
		receiveScript: []byte{0x51},
		sendSessionID: "claim-session",
	}

	client := NewSwapClient(serverConn, daemonConn, nil, creator)
	client.waitPollInterval = time.Millisecond
	client.waitVHTLCTimeout = 50 * time.Millisecond

	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)
	require.Equal(t, "lnrtest1swap", session.Invoice)
	require.Equal(t, hash, session.PaymentHash)

	result, err := session.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "txid:0", result.VTXOOutpoint)
	require.EqualValues(t, 42_000, result.AmountSat)

	require.Len(t, daemonConn.lastClaimInput, 1)
	require.Equal(t, "txid:0", daemonConn.lastClaimInput[0].Outpoint)
	require.NotEmpty(t, daemonConn.lastClaimInput[0].SpendWitnessScript)
	require.NotEmpty(t, daemonConn.lastClaimInput[0].SpendControlBlock)
	require.NotEmpty(t, daemonConn.lastClaimInput[0].ConditionWitness)
}
