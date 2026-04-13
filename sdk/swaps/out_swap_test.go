package swaps

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
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
	preimage *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash, error) {

	if preimage != nil {
		c.paymentHash = lntypes.Hash(
			sha256.Sum256(preimage[:]),
		)
	}

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
	identityKey     *btcec.PublicKey
	operatorKey     *btcec.PublicKey
	liveVTXOs       []VTXOInfo
	spentVTXOs      []VTXOInfo
	vhtlc           *VTXOInfo
	spentVTXO       *VTXOInfo
	indexedPackage  *OORPackageInfo
	receiveInfo     *OORReceiveInfo
	sendSessionID   string
	lastSendPolicy  []byte
	lastClaimPubKey []byte
	lastClaimInput  []CustomInput
}

// SendOORWithPolicy records the requested output policy template.
func (d *testDaemonConn) SendOORWithPolicy(_ context.Context,
	_ int64, recipientPolicyTemplate []byte) (string, error) {

	d.lastSendPolicy = append(
		[]byte(nil), recipientPolicyTemplate...,
	)

	return d.sendSessionID, nil
}

// SendOORWithCustomInputs records the claim request.
func (d *testDaemonConn) SendOORWithCustomInputs(_ context.Context,
	recipientPubKey []byte, _ int64, inputs []CustomInput) (string,
	error) {

	d.lastClaimPubKey = append([]byte(nil), recipientPubKey...)
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
	return append([]VTXOInfo(nil), d.liveVTXOs...), nil
}

// ListSpentVTXOs returns the configured local spent VTXOs.
func (d *testDaemonConn) ListSpentVTXOs(context.Context) ([]VTXOInfo, error) {
	return append([]VTXOInfo(nil), d.spentVTXOs...), nil
}

// FindLiveVTXOByPkScript returns the preconfigured vHTLC.
func (d *testDaemonConn) FindLiveVTXOByPkScript(context.Context,
	[]byte) (*VTXOInfo, error) {

	return d.vhtlc, nil
}

// FindSpentVTXOByPkScript is unused in these tests.
func (d *testDaemonConn) FindSpentVTXOByPkScript(context.Context,
	[]byte) (*VTXOInfo, error) {

	return d.spentVTXO, nil
}

// GetIndexedOORSessionByTxid returns the preconfigured indexed package.
func (d *testDaemonConn) GetIndexedOORSessionByTxid(context.Context,
	[]byte, string) (*OORPackageInfo, error) {

	return d.indexedPackage, nil
}

// NewOORReceiveScript returns the configured receive info.
func (d *testDaemonConn) NewOORReceiveScript(
	context.Context) (*OORReceiveInfo, error) {

	if d.receiveInfo == nil {
		return nil, nil
	}

	return &OORReceiveInfo{
		PkScript: append([]byte(nil), d.receiveInfo.PkScript...),
		PubKey:   append([]byte(nil), d.receiveInfo.PubKey...),
	}, nil
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

	creator := &testInvoiceCreator{
		invoice: &invoices.Invoice{
			PaymentRequest: []byte("lnrtest1swap"),
		},
		paymentHash: lntypes.Hash{},
	}
	serverPubKey := serverPriv.PubKey().SerializeCompressed()

	serverConn := &testSwapServerConn{
		hint: &RouteHint{
			NodeID:          serverPubKey,
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
			SwapServerPubkey:                     serverPubKey,
		},
	}

	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
		vhtlc:       &VTXOInfo{Outpoint: "txid:0", AmountSat: 42_000},
		receiveInfo: &OORReceiveInfo{
			PkScript: []byte{0x51},
			PubKey: schnorr.SerializePubKey(
				clientPriv.PubKey(),
			),
		},
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
	require.Equal(
		t, lntypes.Hash(sha256.Sum256(session.Preimage[:])),
		session.PaymentHash,
	)

	result, err := session.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "txid:0", result.VTXOOutpoint)
	require.EqualValues(t, 42_000, result.AmountSat)

	require.Len(t, daemonConn.lastClaimInput, 1)
	require.Equal(t, "txid:0", daemonConn.lastClaimInput[0].Outpoint)
	require.NotEmpty(t, daemonConn.lastClaimInput[0].VTXOPolicyTemplate)
	require.NotEmpty(t, daemonConn.lastClaimInput[0].SpendPath)
	require.Equal(
		t, daemonConn.receiveInfo.PubKey, daemonConn.lastClaimPubKey,
	)
}
