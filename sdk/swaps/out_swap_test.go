package swaps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
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

// TestStartReceiveRejectsInvalidAmount verifies invalid amounts are rejected
// before the SDK requests route metadata or creates an invoice.
func TestStartReceiveRejectsInvalidAmount(t *testing.T) {
	t.Parallel()

	client := &SwapClient{}

	_, err := client.StartReceiveViaLightning(t.Context(), 0)
	require.ErrorContains(t, err, "receive amount must be positive")

	_, err = client.StartReceiveViaLightning(t.Context(), -1)
	require.ErrorContains(t, err, "receive amount must be positive")

	_, err = client.StartReceiveViaLightning(
		t.Context(), btcutil.MaxSatoshi+1,
	)
	require.ErrorContains(t, err, "exceeds max bitcoin supply")
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
	blockHeight     uint32
	liveVTXOs       []VTXOInfo
	spentVTXOs      []VTXOInfo
	vhtlc           *VTXOInfo
	liveByPkScript  map[string]*VTXOInfo
	spentVTXO       *VTXOInfo
	indexedPackage  *OORPackageInfo
	indexedPackages []*OORPackageInfo
	receiveInfo     *ReceiveInfo
	sendSessionID   string
	sendPolicyErr   error
	sendCustomErr   error
	listSpentErr    error
	spendOnCustom   bool
	sendPolicyCalls int
	sendCustomCalls int
	lastSendPolicy  []byte
	lastClaimPubKey []byte
	lastClaimInput  []CustomInput
}

// BlockHeight returns the configured best block height.
func (d *testDaemonConn) BlockHeight(context.Context) (uint32, error) {
	return d.blockHeight, nil
}

// SendOORWithPolicy records the requested output policy template.
func (d *testDaemonConn) SendOORWithPolicy(_ context.Context,
	_ int64, recipientPolicyTemplate []byte) (string, error) {

	d.sendPolicyCalls++
	d.lastSendPolicy = append(
		[]byte(nil), recipientPolicyTemplate...,
	)

	return d.sendSessionID, d.sendPolicyErr
}

// SendOORWithCustomInputs records the claim request.
func (d *testDaemonConn) SendOORWithCustomInputs(_ context.Context,
	recipientPubKey []byte, _ int64, inputs []CustomInput) (string,
	error) {

	d.sendCustomCalls++
	d.lastClaimPubKey = append([]byte(nil), recipientPubKey...)
	d.lastClaimInput = append([]CustomInput(nil), inputs...)
	if d.spendOnCustom && len(inputs) > 0 {
		d.spentVTXO = &VTXOInfo{
			Outpoint:    inputs[0].Outpoint,
			AmountSat:   inputs[0].AmountSat,
			SpentByTxID: d.sendSessionID,
		}
		d.vhtlc = nil

		pubKey, err := schnorr.ParsePubKey(recipientPubKey)
		if err == nil {
			pkScript, scriptErr := txscript.PayToTaprootScript(
				pubKey,
			)
			if scriptErr == nil {
				if d.liveByPkScript == nil {
					d.liveByPkScript = make(
						map[string]*VTXOInfo,
					)
				}

				d.liveByPkScript[hex.EncodeToString(pkScript)] =
					&VTXOInfo{
						Outpoint: d.sendSessionID +
							":0",
						AmountSat: inputs[0].AmountSat,
						PkScript:  pkScript,
					}
			}
		}
	}

	return d.sendSessionID, d.sendCustomErr
}

// IdentityPubKey returns the configured client key.
func (d *testDaemonConn) IdentityPubKey(context.Context) (
	*btcec.PublicKey, error) {

	return d.identityKey, nil
}

// OperatorPubKey returns the configured operator key.
func (d *testDaemonConn) OperatorPubKey(context.Context) (
	*btcec.PublicKey, error) {

	return d.operatorKey, nil
}

// ListLiveVTXOs is unused in these tests.
func (d *testDaemonConn) ListLiveVTXOs(context.Context) ([]VTXOInfo, error) {
	return append([]VTXOInfo(nil), d.liveVTXOs...), nil
}

// ListSpentVTXOs returns the configured local spent VTXOs.
func (d *testDaemonConn) ListSpentVTXOs(context.Context) ([]VTXOInfo, error) {
	return append([]VTXOInfo(nil), d.spentVTXOs...), d.listSpentErr
}

// FindLiveVTXOByPkScript returns the preconfigured vHTLC.
func (d *testDaemonConn) FindLiveVTXOByPkScript(_ context.Context,
	pkScript []byte) (*VTXOInfo, error) {

	if d.liveByPkScript != nil {
		scriptKey := hex.EncodeToString(pkScript)
		if vtxo := d.liveByPkScript[scriptKey]; vtxo != nil {
			return vtxo, nil
		}
	}

	return d.vhtlc, nil
}

// FindSpentVTXOByPkScript is unused in these tests.
func (d *testDaemonConn) FindSpentVTXOByPkScript(context.Context,
	[]byte) (*VTXOInfo, error) {

	return d.spentVTXO, nil
}

// GetIndexedOORSession returns the preconfigured indexed package.
func (d *testDaemonConn) GetIndexedOORSession(context.Context,
	[]byte, string) (*OORPackageInfo, error) {

	if len(d.indexedPackages) > 0 {
		pkg := d.indexedPackages[0]
		d.indexedPackages = d.indexedPackages[1:]

		return pkg, nil
	}

	return d.indexedPackage, nil
}

// AllocateReceiveScript returns the configured receive info.
func (d *testDaemonConn) AllocateReceiveScript(context.Context,
	string) (*ReceiveInfo, error) {

	if d.receiveInfo == nil {
		return nil, nil
	}

	return &ReceiveInfo{
		PkScript:    append([]byte(nil), d.receiveInfo.PkScript...),
		PubKeyXOnly: append([]byte(nil), d.receiveInfo.PubKeyXOnly...),
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
		receiveInfo: &ReceiveInfo{
			PkScript:    []byte{0x51},
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
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
	require.Equal(t, daemonConn.receiveInfo.PubKeyXOnly,
		daemonConn.lastClaimPubKey)
}

// TestReceiveSessionResumeFromStore asserts the SDK can reload a persisted
// receive session from the isolated swap database and finish the claim path.
func TestReceiveSessionResumeFromStore(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

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
		receiveInfo: &ReceiveInfo{
			PkScript:    []byte{0x51},
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
		},
		sendSessionID: "claim-session",
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)

	daemonConn.vhtlc = &VTXOInfo{
		Outpoint:  "resume-txid:1",
		AmountSat: 42_000,
	}

	resumedClient := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	resumedClient.waitPollInterval = time.Millisecond

	resumed, err := resumedClient.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateInvoiceCreated, resumed.State())

	result, err := resumed.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "resume-txid:1", result.VTXOOutpoint)
	require.EqualValues(t, 42_000, result.AmountSat)
	require.Len(t, daemonConn.lastClaimInput, 1)
}

// TestReceiveSessionCancelDoesNotPersistFailed asserts caller cancellation
// does not durably mark a persisted receive session as Failed.
func TestReceiveSessionCancelDoesNotPersistFailed(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

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
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(
		t.Context(), 5*time.Millisecond,
	)
	defer cancel()

	_, err = session.Wait(waitCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	resumedClient := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	resumed, err := resumedClient.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateInvoiceCreated, resumed.State())
}

// TestReceiveSessionFailsOnAmountMismatch asserts the client stops with an
// ordinary terminal failure when the funded vHTLC amount does not match the
// invoice amount it requested.
func TestReceiveSessionFailsOnAmountMismatch(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

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
		blockHeight: 100,
		vhtlc: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: 41_999,
		},
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	client.waitPollInterval = time.Millisecond
	client.waitVHTLCTimeout = 50 * time.Millisecond

	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)

	_, err = session.Wait(t.Context())
	require.ErrorContains(t, err, "does not match invoice amount")

	resumed, err := client.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateFailed, resumed.State())
	require.Contains(t, resumed.TerminalReason(),
		"does not match invoice amount")
	require.Empty(t, resumed.InterventionReason())
	require.Equal(t, "funding:0", resumed.vhtlcOutpoint)
	require.EqualValues(t, 41_999, resumed.vhtlcAmount)
}

// TestReceiveSessionWaitReconcilesBeforeExpiry asserts a funded vHTLC wins over
// the local receive deadline so index lag does not falsely expire the session.
func TestReceiveSessionWaitReconcilesBeforeExpiry(t *testing.T) {
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
		blockHeight: 100,
		vhtlc: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: 42_000,
		},
		receiveInfo: &ReceiveInfo{
			PkScript:    []byte{0x51},
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
		},
		sendSessionID: "claim-session",
	}

	client := NewSwapClient(serverConn, daemonConn, nil, creator)
	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)
	session.deadline = time.Now().Add(-time.Second)

	result, err := session.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "funding:0", result.VTXOOutpoint)
	require.EqualValues(t, 42_000, result.AmountSat)
}

// TestReceiveSessionClaimFailsOnAmountMismatch asserts manual claims use the
// same amount guard as automatic funding observation.
func TestReceiveSessionClaimFailsOnAmountMismatch(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

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
		blockHeight: 100,
		receiveInfo: &ReceiveInfo{
			PkScript:    []byte{0x51},
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
		},
		sendSessionID: "claim-session",
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)

	_, err = session.Claim(t.Context(), "funding:0", 41_999)
	require.ErrorContains(t, err, "does not match invoice amount")
	require.Equal(t, ReceiveStateFailed, session.State())
	require.Contains(t, session.TerminalReason(),
		"does not match invoice amount")
	require.Empty(t, session.InterventionReason())
	require.Zero(t, daemonConn.sendCustomCalls)
}

// TestReceiveSessionClaimIDPreventsDuplicateClaim asserts a resumed
// ClaimInitiated session with an accepted claim id completes without
// submitting another custom-input spend.
func TestReceiveSessionClaimIDPreventsDuplicateClaim(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

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
		blockHeight: 100,
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)
	err = session.mutateAndPersist(t.Context(), func() error {
		session.vhtlcOutpoint = "funding:0"
		session.vhtlcAmount = 42_000
		return session.transition(receiveEventVHTLCFunded)
	})
	require.NoError(t, err)
	err = session.mutateAndPersist(t.Context(), func() error {
		session.claimSessionID = "claim-session"
		return session.transition(receiveEventClaimInitiated)
	})
	require.NoError(t, err)

	resumed, err := client.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	result, err := resumed.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "funding:0", result.VTXOOutpoint)
	require.Zero(t, daemonConn.sendCustomCalls)
}

// TestReceiveSessionClaimReturnsLastSendError asserts claim retry exhaustion
// reports the daemon's last send error instead of a stale outer error.
func TestReceiveSessionClaimReturnsLastSendError(t *testing.T) {
	t.Parallel()

	senderPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	receiverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:   senderPriv.PubKey(),
		Receiver: receiverPriv.PubKey(),
		Server:   operatorPriv.PubKey(),
		PreimageHash: lntypes.Hash(
			sha256.Sum256(preimage[:]),
		),
		RefundLocktime:                       144,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                24,
		UnilateralRefundWithoutReceiverDelay: 36,
	})
	require.NoError(t, err)

	policyTemplate, err := encodeVHTLCPolicyTemplate(policy)
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	sendErr := errors.New("daemon send failed")
	daemonConn := &testDaemonConn{
		receiveInfo: &ReceiveInfo{
			PkScript:    []byte{0x51},
			PubKeyXOnly: receiverPriv.PubKey().X().Bytes(),
		},
		sendCustomErr: sendErr,
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	client.claimMaxAttempts = 2
	client.claimRetryDelay = time.Nanosecond

	_, err = client.claimReceiveVHTLC(
		t.Context(), preimage.Hash(), preimage, policy,
		policyTemplate, pkScript, "funding:0", 42_000,
	)
	require.ErrorIs(t, err, sendErr)
	require.ErrorContains(t, err, "claim vHTLC")
	require.NotContains(t, err.Error(), "<nil>")
	require.Equal(t, 2, daemonConn.sendCustomCalls)
}
