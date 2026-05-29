package swaps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type testInvoiceCreator struct {
	invoice     *invoices.Invoice
	paymentHash lntypes.Hash
	lastAuthKey keychain.SingleKeyMessageSigner
	lastMemo    string
	authKeys    []keychain.SingleKeyMessageSigner
}

// CreateInvoice returns the preconfigured invoice and payment hash.
func (c *testInvoiceCreator) CreateInvoice(_ context.Context, _ btcutil.Amount,
	memo string, _ *RouteHint, _ time.Duration,
	preimage *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash, error) {

	c.lastMemo = memo
	if preimage != nil {
		c.paymentHash = lntypes.Hash(
			sha256.Sum256(preimage[:]),
		)
	}

	return c.invoice, c.paymentHash, nil
}

// CreateInvoiceWithKey returns the preconfigured invoice and payment hash.
func (c *testInvoiceCreator) CreateInvoiceWithKey(ctx context.Context,
	amount btcutil.Amount, memo string, hint *RouteHint,
	expiry time.Duration, authKey keychain.SingleKeyMessageSigner,
	preimage *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash, error) {

	c.lastAuthKey = authKey
	c.authKeys = append(c.authKeys, authKey)

	return c.CreateInvoice(ctx, amount, memo, hint, expiry, preimage)
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

// TestStartReceiveRejectsPayerFeeOverflow verifies quoted route fees must fit
// the signed SQLite column before a receive session can be persisted.
func TestStartReceiveRejectsPayerFeeOverflow(t *testing.T) {
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
		payerFeeMsat: uint64(math.MaxInt64) + 1,
	}
	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, newTestSwapStore(t),
	)
	_, err = client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.ErrorContains(t, err, "payer fee")
	require.ErrorContains(t, err, "overflows int64")
}

// TestStartReceiveDerivesReceiveAuthKeyPerPaymentHash verifies new receive
// sessions use payment-scoped auth keys.
func TestStartReceiveDerivesReceiveAuthKeyPerPaymentHash(t *testing.T) {
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
	}
	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
	}

	client := NewSwapClient(serverConn, daemonConn, nil, creator)
	for i := 0; i < 2; i++ {
		_, err := client.StartReceiveViaLightning(
			t.Context(), btcutil.Amount(42_000),
			"coffee",
		)
		require.NoError(t, err)
	}

	require.Equal(t, "coffee", creator.lastMemo)
	require.NotNil(t, serverConn.lastVhtlcPubkey)
	require.True(t, serverConn.lastVhtlcPubkey.IsEqual(clientPriv.PubKey()))
	require.Len(t, creator.authKeys, 2)
	require.NotSame(t, creator.authKeys[0], creator.authKeys[1])
	require.False(
		t,
		creator.authKeys[0].PubKey().IsEqual(
			creator.authKeys[1].PubKey(),
		),
	)
}

// TestAcceptInArkHtlcEventBuildsSenderReceiverPolicy verifies that same-Ark
// receive events are validated directly without requiring a Lightning onion.
func TestAcceptInArkHtlcEventBuildsSenderReceiverPolicy(t *testing.T) {
	t.Parallel()

	senderPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	receiverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage := lntypes.Preimage{1, 2, 3}
	hash := preimage.Hash()
	cfg := VHTLCConfig{
		RefundLocktime:                       900,
		UnilateralClaimDelay:                 5,
		UnilateralRefundDelay:                6,
		UnilateralRefundWithoutReceiverDelay: 7,
		SwapServerPubkey: senderPriv.PubKey().
			SerializeCompressed(),
	}
	session := &ReceiveSession{
		client: &SwapClient{
			daemon: &testDaemonConn{},
		},
		amountSat:      btcutil.Amount(42_000),
		state:          ReceiveStateInvoiceCreated,
		PaymentHash:    hash,
		clientPubKey:   receiverPriv.PubKey(),
		operatorPubKey: operatorPriv.PubKey(),
	}

	err = session.acceptInArkHtlcEvent(t.Context(), &InArkHtlcEvent{
		PaymentHash:  hash,
		AmountSat:    42_000,
		SenderPubkey: senderPriv.PubKey(),
		VHTLCConfig:  cfg,
	}, 0)
	require.NoError(t, err)

	refundNoReceiverDelay := cfg.UnilateralRefundWithoutReceiverDelay
	expected, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:                               senderPriv.PubKey(),
		Receiver:                             receiverPriv.PubKey(),
		Server:                               operatorPriv.PubKey(),
		PreimageHash:                         hash,
		RefundLocktime:                       cfg.RefundLocktime,
		UnilateralClaimDelay:                 cfg.UnilateralClaimDelay,
		UnilateralRefundDelay:                cfg.UnilateralRefundDelay,
		UnilateralRefundWithoutReceiverDelay: refundNoReceiverDelay,
	})
	require.NoError(t, err)

	expectedScript, err := expected.PkScript()
	require.NoError(t, err)
	require.Equal(t, expectedScript, session.vhtlcPkScript)
	require.True(t, session.swapServerPubKey.IsEqual(senderPriv.PubKey()))
}

type testSwapServerConn struct {
	hint          *RouteHint
	payerFeeMsat  uint64
	cfg           *VHTLCConfig
	htlcAmountSat uint64
	waitErr       error
	ackErr        error
	ackErrs       []error
	ackCursor     uint64
	waitCalls     int
	ackCalls      int
	lastAckCursor uint64

	lastVhtlcPubkey *btcec.PublicKey
	lastAmountSat   btcutil.Amount
}

type testIncomingEventReceiver struct {
	notification *IncomingVHTLCNotification
}

type blockingOutSwapEventReceiver struct{}

func (r *blockingOutSwapEventReceiver) WaitOutSwapHtlc(ctx context.Context,
	_ lntypes.Hash, _ *btcec.PublicKey) (*OutSwapHtlcNotification, error) {

	<-ctx.Done()

	return nil, ctx.Err()
}

func (r *blockingOutSwapEventReceiver) AckOutSwapHtlc(_ context.Context,
	_ lntypes.Hash, _ *btcec.PublicKey, _ uint64) error {

	return fmt.Errorf("unexpected out-swap ack")
}

// WaitOutSwapHtlc is unused when the incoming vHTLC path is available.
func (r *testIncomingEventReceiver) WaitOutSwapHtlc(context.Context,
	lntypes.Hash, *btcec.PublicKey) (*OutSwapHtlcNotification, error) {

	return nil, fmt.Errorf("unexpected out-swap wait")
}

// AckOutSwapHtlc is unused when the incoming vHTLC path is available.
func (r *testIncomingEventReceiver) AckOutSwapHtlc(context.Context,
	lntypes.Hash, *btcec.PublicKey, uint64) error {

	return fmt.Errorf("unexpected out-swap ack")
}

// WaitIncomingVHTLC returns the configured incoming vHTLC notification.
func (r *testIncomingEventReceiver) WaitIncomingVHTLC(context.Context,
	lntypes.Hash, *btcec.PublicKey) (*IncomingVHTLCNotification, error) {

	return r.notification, nil
}

// RequestChannelID returns the preconfigured out-swap route hint.
func (c *testSwapServerConn) RequestChannelID(_ context.Context,
	vhtlcPubkey *btcec.PublicKey, _ lntypes.Hash, amountSat btcutil.Amount,
	_ uint32) (*OutSwapQuote, error) {

	c.lastVhtlcPubkey = vhtlcPubkey
	c.lastAmountSat = amountSat

	return &OutSwapQuote{
		RouteHint:        c.hint,
		ReceiveAmountSat: amountSat,
		PayerFeeMsat:     c.payerFeeMsat,
	}, nil
}

// WaitOutSwapHtlc returns the preconfigured out-swap HTLC event.
func (c *testSwapServerConn) WaitOutSwapHtlc(_ context.Context,
	hash lntypes.Hash, _ *btcec.PublicKey) (*OutSwapHtlcNotification,
	error) {

	c.waitCalls++
	if c.waitErr != nil {
		return nil, c.waitErr
	}

	amountSat := c.htlcAmountSat
	if amountSat == 0 {
		amountSat = 42_000
	}
	ackCursor := c.ackCursor
	if ackCursor == 0 {
		ackCursor = 8
	}

	return &OutSwapHtlcNotification{
		Event: &OutSwapHtlcEvent{
			PaymentHash: hash,
			AmountSat:   int64(amountSat),
			VHTLCConfig: *c.cfg,
		},
		AckCursor: ackCursor,
		Ack: func(ctx context.Context) error {
			return c.AckOutSwapHtlc(
				ctx, hash, nil, ackCursor,
			)
		},
	}, nil
}

// AckOutSwapHtlc records the preconfigured out-swap HTLC ack request.
func (c *testSwapServerConn) AckOutSwapHtlc(_ context.Context, _ lntypes.Hash,
	_ *btcec.PublicKey, cursor uint64) error {

	c.ackCalls++
	c.lastAckCursor = cursor
	if len(c.ackErrs) > 0 {
		err := c.ackErrs[0]
		c.ackErrs = c.ackErrs[1:]

		return err
	}

	return c.ackErr
}

// CreateInSwap is unused in these tests.
func (c *testSwapServerConn) CreateInSwap(context.Context, string, uint64,
	*btcec.PublicKey) (*InSwapConfig, error) {

	return nil, nil
}

// AuthorizeInSwapRefund is unused in these tests.
func (c *testSwapServerConn) AuthorizeInSwapRefund(context.Context,
	lntypes.Hash, string, int64, []byte, []byte, []byte) (
	*InSwapRefundAuthorization, error) {

	return nil, fmt.Errorf("unexpected in-swap refund authorization")
}

// Close closes the server connection.
func (c *testSwapServerConn) Close() error {
	return nil
}

// useTestOnionDecoder installs a deterministic final-hop onion decoder for
// unit tests that exercise receive lifecycle behavior with a fake server.
func useTestOnionDecoder(client *SwapClient, amount btcutil.Amount) {
	client.decodeOutSwapOnion = func(ReceiveAuthKey, lntypes.Hash, []byte) (
		*decodedOutSwapOnion, error) {

		msat := lnwire.NewMSatFromSatoshis(amount)

		return &decodedOutSwapOnion{
			amountToForward: msat,
			totalAmount:     msat,
			hasMPP:          true,
		}, nil
	}
}

// acceptTestOutSwapHtlcEvent moves a receive session through the durable
// mailbox-event boundary without waiting for the test server receiver.
func acceptTestOutSwapHtlcEvent(t *testing.T, client *SwapClient,
	session *ReceiveSession, cfg VHTLCConfig) {

	t.Helper()

	authKey, err := client.receiveAuthKey(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)

	err = session.acceptOutSwapHtlcEvent(
		t.Context(),
		&OutSwapHtlcEvent{
			PaymentHash: session.PaymentHash,
			AmountSat:   int64(session.amountSat),
			VHTLCConfig: cfg,
		},
		authKey,
		0,
	)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateHTLCEventAccepted, session.State())
}

// TestWaitIncomingVHTLCNotificationRejectsMissingAckCursor verifies ack
// metadata is checked before the event is durably accepted.
func TestWaitIncomingVHTLCNotificationRejectsMissingAckCursor(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ack := func(context.Context) error {
		return nil
	}
	session := &ReceiveSession{
		client: &SwapClient{
			outEvents: &testIncomingEventReceiver{
				notification: &IncomingVHTLCNotification{
					OutSwap: &OutSwapHtlcEvent{},
					Ack:     ack,
				},
			},
		},
		state:        ReceiveStateInvoiceCreated,
		clientPubKey: clientPriv.PubKey(),
	}

	_, err = session.waitIncomingVHTLCNotification(t.Context(), nil)
	require.ErrorContains(t, err, "incoming vHTLC ack cursor")
	require.Equal(t, ReceiveStateInvoiceCreated, session.State())
	require.Zero(t, session.pendingHTLCAckCursor)
}

// TestCancelVHTLCRecoveryIgnoresNotFound verifies stale local swap metadata can
// forget a recovery id that no longer exists in the daemon's durable recovery
// table.
func TestCancelVHTLCRecoveryIgnoresNotFound(t *testing.T) {
	t.Parallel()

	daemonConn := &testDaemonConn{
		cancelErr: status.Error(codes.NotFound, "missing recovery"),
	}

	err := cancelVHTLCRecovery(
		t.Context(), daemonConn, "missing-recovery",
		recoveryReasonClaimAccepted, "",
	)
	require.NoError(t, err)
	require.Equal(t, 1, daemonConn.cancelCalls)
	require.Equal(
		t, "missing-recovery", daemonConn.lastCancel.GetRecoveryId(),
	)
}

// TestWaitForVHTLCFallsBackToLocalVTXOOnUnregisteredScript verifies same-Ark
// p2p receives can proceed from the daemon-local OOR materialization path when
// the proof-gated indexer lookup is not registered for the receiver principal.
func TestWaitForVHTLCFallsBackToLocalVTXOOnUnregisteredScript(t *testing.T) {
	t.Parallel()

	pkScript := bytes.Repeat([]byte{0x02}, 34)
	localVTXO := VTXOInfo{
		Outpoint:  "local-vhtlc:0",
		AmountSat: 42_000,
		PkScript:  pkScript,
	}
	daemonConn := &testDaemonConn{
		liveVTXOs: []VTXOInfo{
			localVTXO,
		},
		liveLookupErr: status.Error(
			codes.Internal, "indexer query failed: rpc error: "+
				"code = Unauthenticated desc = script not "+
				"registered for principal",
		),
	}
	client := NewSwapClient(nil, daemonConn, nil, nil)

	outpoint, amount, err := client.waitForVHTLC(
		t.Context(), pkScript, time.Time{}, nil,
	)
	require.NoError(t, err)
	require.Equal(t, localVTXO.Outpoint, outpoint)
	require.Equal(t, localVTXO.AmountSat, amount)
	require.Equal(t, 1, daemonConn.liveLookupCalls)
}

type testDaemonConn struct {
	identityKey       *btcec.PublicKey
	operatorKey       *btcec.PublicKey
	blockHeight       uint32
	liveVTXOs         []VTXOInfo
	spentVTXOs        []VTXOInfo
	vhtlc             *VTXOInfo
	liveByPkScript    map[string]*VTXOInfo
	spentVTXO         *VTXOInfo
	indexedPackage    *OORPackageInfo
	indexedPackages   []*OORPackageInfo
	receiveInfo       *ReceiveInfo
	receiveAuthKey    []byte
	receiveAuthErr    error
	receiveAllocCalls int
	sendSessionID     string
	sendOutpoint      string
	preparedOOR       *PreparedOOR
	prepareOOREErr    error
	sendPolicyErr     error
	sendCustomErr     error
	listSpentErr      error
	liveLookupErr     error
	spentLookupErr    error
	spentLookupBlock  time.Duration
	spendOnCustom     bool
	sendPolicyCalls   int
	sendCustomCalls   int
	liveLookupCalls   int
	spentLookupCalls  int
	lastSendPolicy    []byte
	lastClaimPubKey   []byte
	lastClaimInput    []CustomInput
	armRecoveryResp   *daemonrpc.ArmVHTLCRecoveryResponse
	armRecoveryErr    error
	escalateResp      *daemonrpc.EscalateVHTLCRecoveryResponse
	escalateErr       error
	cancelResp        *daemonrpc.CancelVHTLCRecoveryResponse
	cancelErr         error
	statusResp        *daemonrpc.GetVHTLCRecoveryStatusResponse
	statusErr         error
	armRecoveryCalls  int
	escalateCalls     int
	cancelCalls       int
	statusCalls       int
	lastArmRecovery   *daemonrpc.ArmVHTLCRecoveryRequest
	lastEscalate      *daemonrpc.EscalateVHTLCRecoveryRequest
	lastCancel        *daemonrpc.CancelVHTLCRecoveryRequest
	lastStatus        *daemonrpc.GetVHTLCRecoveryStatusRequest
}

// BlockHeight returns the configured best block height.
func (d *testDaemonConn) BlockHeight(context.Context) (uint32, error) {
	return d.blockHeight, nil
}

// SendOORWithPolicyDetails records the requested output policy template.
func (d *testDaemonConn) SendOORWithPolicyDetails(_ context.Context, _ int64,
	recipientPolicyTemplate []byte) (*OORSendResult, error) {

	d.sendPolicyCalls++
	d.lastSendPolicy = append(
		[]byte(nil), recipientPolicyTemplate...,
	)

	if d.sendPolicyErr != nil {
		return nil, d.sendPolicyErr
	}

	return &OORSendResult{
		SessionID:         d.sendSessionID,
		RecipientOutpoint: d.sendOutpoint,
	}, nil
}

// SendOORWithCustomInputs records the claim request.
func (d *testDaemonConn) SendOORWithCustomInputs(_ context.Context,
	recipientPubKey []byte, _ int64, inputs []CustomInput) (string, error) {

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

// ArmVHTLCRecovery records the daemon recovery arm request.
func (d *testDaemonConn) ArmVHTLCRecovery(_ context.Context,
	req *daemonrpc.ArmVHTLCRecoveryRequest) (
	*daemonrpc.ArmVHTLCRecoveryResponse, error) {

	d.armRecoveryCalls++
	d.lastArmRecovery = req
	if d.armRecoveryErr != nil {
		return nil, d.armRecoveryErr
	}
	if d.armRecoveryResp != nil {
		return d.armRecoveryResp, nil
	}

	return &daemonrpc.ArmVHTLCRecoveryResponse{
		RecoveryId: fmt.Sprintf("recovery-%d", d.armRecoveryCalls),
		Created:    true,
		Status: &daemonrpc.VHTLCRecoveryStatus{
			RecoveryId: fmt.Sprintf(
				"recovery-%d", d.armRecoveryCalls,
			),
			State: recoveryStateArmed,
		},
	}, nil
}

// EscalateVHTLCRecovery records the daemon recovery escalation request.
func (d *testDaemonConn) EscalateVHTLCRecovery(_ context.Context,
	req *daemonrpc.EscalateVHTLCRecoveryRequest) (
	*daemonrpc.EscalateVHTLCRecoveryResponse, error) {

	d.escalateCalls++
	d.lastEscalate = req
	if d.escalateErr != nil {
		return nil, d.escalateErr
	}
	if d.escalateResp != nil {
		return d.escalateResp, nil
	}

	return &daemonrpc.EscalateVHTLCRecoveryResponse{
		Status: &daemonrpc.VHTLCRecoveryStatus{
			RecoveryId: req.GetRecoveryId(),
			State:      recoveryStateUnrollStarted,
		},
	}, nil
}

// CancelVHTLCRecovery records the daemon recovery cancellation request.
func (d *testDaemonConn) CancelVHTLCRecovery(_ context.Context,
	req *daemonrpc.CancelVHTLCRecoveryRequest) (
	*daemonrpc.CancelVHTLCRecoveryResponse, error) {

	d.cancelCalls++
	d.lastCancel = req
	if d.cancelErr != nil {
		return nil, d.cancelErr
	}
	if d.cancelResp != nil {
		return d.cancelResp, nil
	}

	return &daemonrpc.CancelVHTLCRecoveryResponse{
		Status: &daemonrpc.VHTLCRecoveryStatus{
			RecoveryId: req.GetRecoveryId(),
			State:      recoveryStateCancelled,
		},
	}, nil
}

// GetVHTLCRecoveryStatus records the daemon recovery status request.
func (d *testDaemonConn) GetVHTLCRecoveryStatus(_ context.Context,
	req *daemonrpc.GetVHTLCRecoveryStatusRequest) (
	*daemonrpc.GetVHTLCRecoveryStatusResponse, error) {

	d.statusCalls++
	d.lastStatus = req
	if d.statusErr != nil {
		return nil, d.statusErr
	}
	if d.statusResp != nil {
		return d.statusResp, nil
	}

	return &daemonrpc.GetVHTLCRecoveryStatusResponse{
		Found: false,
	}, nil
}

// PrepareOORWithCustomInputs records the requested package and returns
// deterministic signing material for tests.
func (d *testDaemonConn) PrepareOORWithCustomInputs(_ context.Context,
	recipientPubKey []byte, _ int64, inputs []CustomInput) (*PreparedOOR,
	error) {

	if d.prepareOOREErr != nil {
		return nil, d.prepareOOREErr
	}
	if d.preparedOOR != nil {
		return d.preparedOOR, nil
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("custom input is required")
	}

	return &PreparedOOR{
		CustomInputs: []PreparedOORCustomInput{{
			Outpoint:       inputs[0].Outpoint,
			CheckpointPSBT: []byte("checkpoint"),
			WitnessScript:  []byte("witness"),
			SigningPubKeys: [][]byte{
				append([]byte(nil), recipientPubKey...),
			},
		}},
		SessionID: d.sendSessionID,
	}, nil
}

// IdentityPubKey returns the configured client key.
func (d *testDaemonConn) IdentityPubKey(context.Context) (*btcec.PublicKey,
	error) {

	return d.identityKey, nil
}

// OperatorPubKey returns the configured operator key.
func (d *testDaemonConn) OperatorPubKey(context.Context) (*btcec.PublicKey,
	error) {

	return d.operatorKey, nil
}

// receiveAuthPrivKey derives the configured deterministic receive-auth key.
func (d *testDaemonConn) receiveAuthPrivKey(paymentHash lntypes.Hash) (
	*btcec.PrivateKey, error) {

	if d.receiveAuthErr != nil {
		return nil, d.receiveAuthErr
	}

	if len(d.receiveAuthKey) == 0 {
		key := sha256.Sum256(
			append(
				[]byte("test receive auth key"),
				paymentHash[:]...,
			),
		)

		privKey, _ := btcec.PrivKeyFromBytes(key[:])

		return privKey, nil
	}

	privKey, _ := btcec.PrivKeyFromBytes(d.receiveAuthKey)

	return privKey, nil
}

// ReceiveAuthKey returns the configured deterministic receive-auth pubkey.
func (d *testDaemonConn) ReceiveAuthKey(_ context.Context,
	paymentHash lntypes.Hash) (*btcec.PublicKey, error) {

	privKey, err := d.receiveAuthPrivKey(paymentHash)
	if err != nil {
		return nil, err
	}

	return privKey.PubKey(), nil
}

// SignReceiveAuthMessage signs a message with the deterministic receive-auth
// key.
func (d *testDaemonConn) SignReceiveAuthMessage(_ context.Context,
	paymentHash lntypes.Hash, message []byte, doubleHash bool) (
	*ecdsa.Signature, error) {

	privKey, err := d.receiveAuthPrivKey(paymentHash)
	if err != nil {
		return nil, err
	}

	return keychain.NewPrivKeyMessageSigner(
		privKey, keychain.KeyLocator{},
	).SignMessage(message, doubleHash)
}

// SignReceiveAuthMessageCompact signs a message with the deterministic
// receive-auth key and returns the compact signature.
func (d *testDaemonConn) SignReceiveAuthMessageCompact(_ context.Context,
	paymentHash lntypes.Hash, message []byte, doubleHash bool) ([]byte,
	error) {

	privKey, err := d.receiveAuthPrivKey(paymentHash)
	if err != nil {
		return nil, err
	}

	return keychain.NewPrivKeyMessageSigner(
		privKey, keychain.KeyLocator{},
	).SignMessageCompact(message, doubleHash)
}

// ReceiveAuthECDH derives one Sphinx shared secret with the deterministic
// receive-auth key.
func (d *testDaemonConn) ReceiveAuthECDH(_ context.Context,
	paymentHash lntypes.Hash, pubKey *btcec.PublicKey) ([32]byte, error) {

	privKey, err := d.receiveAuthPrivKey(paymentHash)
	if err != nil {
		return [32]byte{}, err
	}

	var pubJ btcec.JacobianPoint
	pubKey.AsJacobian(&pubJ)

	var ecdhPoint btcec.JacobianPoint
	btcec.ScalarMultNonConst(&privKey.Key, &pubJ, &ecdhPoint)

	ecdhPoint.ToAffine()
	ecdhPubKey := btcec.NewPublicKey(&ecdhPoint.X, &ecdhPoint.Y)

	return sha256.Sum256(ecdhPubKey.SerializeCompressed()), nil
}

// ListLiveVTXOs returns configured daemon-local live VTXOs.
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

	d.liveLookupCalls++
	if d.liveLookupErr != nil {
		return nil, d.liveLookupErr
	}

	if d.liveByPkScript != nil {
		scriptKey := hex.EncodeToString(pkScript)
		if vtxo := d.liveByPkScript[scriptKey]; vtxo != nil {
			return vtxo, nil
		}
	}
	if d.receiveInfo != nil &&
		bytes.Equal(d.receiveInfo.PkScript, pkScript) {
		return nil, nil
	}
	if d.vhtlc != nil && len(d.vhtlc.PkScript) != 0 &&
		!bytes.Equal(d.vhtlc.PkScript, pkScript) {
		return nil, nil
	}

	return d.vhtlc, nil
}

// FindSpentVTXOByPkScript returns the configured spent vHTLC. When
// spentLookupBlock is set, the call waits for the caller's context to expire
// before returning the configured error so test cases can exercise the bounded
// reconcile timeout.
func (d *testDaemonConn) FindSpentVTXOByPkScript(ctx context.Context,
	_ []byte) (*VTXOInfo, error) {

	d.spentLookupCalls++

	if d.spentLookupBlock > 0 {
		timer := time.NewTimer(d.spentLookupBlock)
		defer timer.Stop()

		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}

	return d.spentVTXO, d.spentLookupErr
}

// GetIndexedOORSession returns the preconfigured indexed package.
func (d *testDaemonConn) GetIndexedOORSession(context.Context, []byte, string) (
	*OORPackageInfo, error) {

	if len(d.indexedPackages) > 0 {
		pkg := d.indexedPackages[0]
		d.indexedPackages = d.indexedPackages[1:]

		return pkg, nil
	}

	return d.indexedPackage, nil
}

// AllocateReceiveScript returns the configured receive info.
func (d *testDaemonConn) AllocateReceiveScript(context.Context, string) (
	*ReceiveInfo, error) {

	d.receiveAllocCalls++
	if d.receiveInfo == nil {
		if d.identityKey == nil {
			return nil, nil
		}

		pkScript, err := txscript.PayToTaprootScript(d.identityKey)
		if err != nil {
			return nil, err
		}

		return &ReceiveInfo{
			PkScript:    pkScript,
			PubKeyXOnly: d.identityKey.X().Bytes(),
		}, nil
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
	authPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

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
		vhtlc: &VTXOInfo{
			Outpoint:  "txid:0",
			AmountSat: 42_000,
		},
		receiveAuthKey: authPrivKey.Serialize(),
		receiveInfo: &ReceiveInfo{
			PkScript: []byte{
				0x51,
			},
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
		},
		sendSessionID: "claim-session",
	}

	client := NewSwapClient(serverConn, daemonConn, nil, creator)
	useTestOnionDecoder(client, 42_000)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)
	require.Equal(t, "lnrtest1swap", session.Invoice)
	require.Equal(
		t,
		lntypes.Hash(
			sha256.Sum256(session.Preimage[:]),
		),
		session.PaymentHash,
	)
	require.True(
		t,
		creator.lastAuthKey.PubKey().IsEqual(
			authPrivKey.PubKey(),
		),
	)
	require.NotNil(t, serverConn.lastVhtlcPubkey)
	require.True(t, serverConn.lastVhtlcPubkey.IsEqual(clientPriv.PubKey()))

	result, err := session.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "txid:0", result.VTXOOutpoint)
	require.EqualValues(t, 42_000, result.AmountSat)

	require.Len(t, daemonConn.lastClaimInput, 1)
	require.Equal(t, "txid:0", daemonConn.lastClaimInput[0].Outpoint)
	require.NotEmpty(t, daemonConn.lastClaimInput[0].VTXOPolicyTemplate)
	require.NotEmpty(t, daemonConn.lastClaimInput[0].SpendPath)
	require.Equal(
		t, daemonConn.receiveInfo.PubKeyXOnly,
		daemonConn.lastClaimPubKey,
	)
}

// TestReceiveSessionVHTLCInfoWaitsForAcceptedEvent asserts callers get a
// clear error instead of a nil-policy panic before the mailbox event arrives.
func TestReceiveSessionVHTLCInfoWaitsForAcceptedEvent(t *testing.T) {
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
	cfg := VHTLCConfig{
		RefundLocktime:                       144,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                24,
		UnilateralRefundWithoutReceiverDelay: 36,
		SwapServerPubkey:                     serverPubKey,
	}
	serverConn := &testSwapServerConn{
		hint: &RouteHint{
			NodeID:          serverPubKey,
			ChannelID:       99,
			FeeBaseMsat:     1,
			FeePropPpm:      2,
			CltvExpiryDelta: 40,
		},
		cfg: &cfg,
	}
	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
	}

	client := NewSwapClient(serverConn, daemonConn, nil, creator)
	useTestOnionDecoder(client, 42_000)

	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)

	_, err = session.VHTLCInfo()
	require.ErrorContains(
		t, err, "out-swap HTLC event has not been accepted yet",
	)

	acceptTestOutSwapHtlcEvent(t, client, session, cfg)

	info, err := session.VHTLCInfo()
	require.NoError(t, err)
	require.NotEmpty(t, info.PkScript)
	require.NotEmpty(t, info.ClaimScript)
}

// TestReceiveSessionRejectsInvalidOnion asserts the SDK refuses to claim a
// mailbox-notified vHTLC when final-hop onion validation fails.
func TestReceiveSessionRejectsInvalidOnion(t *testing.T) {
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
		vhtlc: &VTXOInfo{
			Outpoint:  "txid:0",
			AmountSat: 42_000,
		},
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	client.decodeOutSwapOnion = func(ReceiveAuthKey, lntypes.Hash, []byte) (
		*decodedOutSwapOnion, error) {

		return nil, errors.New("bad onion")
	}

	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)

	_, err = session.Wait(t.Context())
	require.ErrorContains(t, err, "out-swap HTLC onion validation failed")
	require.Equal(t, ReceiveStateFailed, session.State())
	require.Zero(t, daemonConn.sendCustomCalls)

	resumed, err := client.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateFailed, resumed.State())
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
		payerFeeMsat: 123_000,
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
			PkScript: []byte{
				0x51,
			},
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
		},
		sendSessionID: "claim-session",
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	useTestOnionDecoder(client, 42_000)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)
	require.EqualValues(t, 42_000, serverConn.lastAmountSat)
	require.EqualValues(t, 123_000, session.payerFeeMsat)
	require.Equal(t, 1, daemonConn.receiveAllocCalls)

	daemonConn.vhtlc = &VTXOInfo{
		Outpoint:  "resume-txid:1",
		AmountSat: 42_000,
	}

	resumedClient := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	useTestOnionDecoder(resumedClient, 42_000)
	resumedClient.waitPollInterval = time.Millisecond

	resumed, err := resumedClient.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateInvoiceCreated, resumed.State())
	require.EqualValues(t, 123_000, resumed.payerFeeMsat)

	result, err := resumed.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "resume-txid:1", result.VTXOOutpoint)
	require.EqualValues(t, 42_000, result.AmountSat)
	require.Len(t, daemonConn.lastClaimInput, 1)
	require.Equal(
		t, daemonConn.receiveInfo.PubKeyXOnly,
		daemonConn.lastClaimPubKey,
	)
	require.Equal(t, 1, daemonConn.receiveAllocCalls)
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
	useTestOnionDecoder(client, 42_000)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(
		t.Context(), 100*time.Millisecond,
	)
	defer cancel()

	_, err = session.Wait(waitCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	resumedClient := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	useTestOnionDecoder(resumedClient, 42_000)
	resumed, err := resumedClient.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateHTLCEventAccepted, resumed.State())
}

// TestReceiveSessionResumesAfterAckedHTLCEvent asserts a receive can recover
// when it crashes after acking the mailbox event but before funding is indexed.
func TestReceiveSessionResumesAfterAckedHTLCEvent(t *testing.T) {
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
	cfg := &VHTLCConfig{
		RefundLocktime:                       144,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                24,
		UnilateralRefundWithoutReceiverDelay: 36,
		SwapServerPubkey:                     serverPubKey,
	}
	serverConn := &testSwapServerConn{
		hint: &RouteHint{
			NodeID:          serverPubKey,
			ChannelID:       99,
			FeeBaseMsat:     1,
			FeePropPpm:      2,
			CltvExpiryDelta: 40,
		},
		cfg: cfg,
	}

	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
		blockHeight: 100,
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	useTestOnionDecoder(client, 42_000)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)

	err = session.waitForHTLCEvent(t.Context())
	require.NoError(t, err)
	require.Equal(t, ReceiveStateHTLCEventAccepted, session.State())
	require.Equal(t, 1, serverConn.waitCalls)
	require.Equal(t, 1, serverConn.ackCalls)
	require.Equal(t, uint64(8), serverConn.lastAckCursor)
	require.Zero(t, session.pendingHTLCAckCursor)

	resumeServer := &testSwapServerConn{
		waitErr: errors.New("unexpected mailbox wait"),
	}
	resumedClient := NewSwapClientWithStore(
		resumeServer, daemonConn, nil, creator, store,
	)
	resumedClient.waitPollInterval = time.Millisecond

	resumed, err := resumedClient.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateHTLCEventAccepted, resumed.State())

	daemonConn.vhtlc = &VTXOInfo{
		Outpoint:  "resume-txid:1",
		AmountSat: 42_000,
	}

	outpoint, amount, err := resumed.WaitForFunding(t.Context())
	require.NoError(t, err)
	require.Equal(t, "resume-txid:1", outpoint)
	require.EqualValues(t, 42_000, amount)
	require.Zero(t, resumeServer.waitCalls)
}

// TestReceiveSessionRetriesAcceptedHTLCAckOnResume asserts mailbox ack errors
// after event acceptance are retried from the durable accepted-event state.
func TestReceiveSessionRetriesAcceptedHTLCAckOnResume(t *testing.T) {
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
	cfg := &VHTLCConfig{
		RefundLocktime:                       144,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                24,
		UnilateralRefundWithoutReceiverDelay: 36,
		SwapServerPubkey:                     serverPubKey,
	}
	ackErr := errors.New("temporary ack failure")
	serverConn := &testSwapServerConn{
		hint: &RouteHint{
			NodeID:          serverPubKey,
			ChannelID:       99,
			FeeBaseMsat:     1,
			FeePropPpm:      2,
			CltvExpiryDelta: 40,
		},
		cfg: cfg,
		ackErrs: []error{
			ackErr,
		},
		ackCursor: 12,
	}

	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
		blockHeight: 100,
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	useTestOnionDecoder(client, 42_000)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)

	err = session.waitForHTLCEvent(t.Context())
	require.ErrorIs(t, err, ackErr)
	require.Equal(t, ReceiveStateHTLCEventAccepted, session.State())
	require.EqualValues(t, 12, session.pendingHTLCAckCursor)
	require.Equal(t, 1, serverConn.waitCalls)
	require.Equal(t, 1, serverConn.ackCalls)
	require.Equal(t, uint64(12), serverConn.lastAckCursor)

	resumeServer := &testSwapServerConn{
		waitErr: errors.New("unexpected mailbox wait"),
	}
	resumedClient := NewSwapClientWithStore(
		resumeServer, daemonConn, nil, creator, store,
	)
	resumedClient.waitPollInterval = time.Millisecond

	resumed, err := resumedClient.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateHTLCEventAccepted, resumed.State())
	require.EqualValues(t, 12, resumed.pendingHTLCAckCursor)

	daemonConn.vhtlc = &VTXOInfo{
		Outpoint:  "resume-txid:1",
		AmountSat: 42_000,
	}

	outpoint, amount, err := resumed.WaitForFunding(t.Context())
	require.NoError(t, err)
	require.Equal(t, "resume-txid:1", outpoint)
	require.EqualValues(t, 42_000, amount)
	require.Zero(t, resumeServer.waitCalls)
	require.Equal(t, 1, resumeServer.ackCalls)
	require.Equal(t, uint64(12), resumeServer.lastAckCursor)
	require.Zero(t, resumed.pendingHTLCAckCursor)

	reloaded, err := resumedClient.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	require.Zero(t, reloaded.pendingHTLCAckCursor)
}

// TestReceiveSessionExpiresAtRefundLocktimeWithoutFunding asserts a resumed
// receive does not wait for the longer invoice deadline after the server-side
// refund window has opened and no live vHTLC can still be claimed.
func TestReceiveSessionExpiresAtRefundLocktimeWithoutFunding(t *testing.T) {
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
	useTestOnionDecoder(client, 42_000)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)

	daemonConn.blockHeight = 143

	resumed, err := client.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateInvoiceCreated, resumed.State())

	_, _, err = resumed.WaitForFunding(t.Context())
	require.ErrorIs(t, err, errSwapExpired)
	require.ErrorContains(
		t, err, "refund locktime 144 is imminent or reached",
	)
	require.Equal(t, ReceiveStateExpired, resumed.State())

	reloaded, err := client.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateExpired, reloaded.State())

	_, err = reloaded.Wait(t.Context())
	require.ErrorIs(t, err, errSwapExpired)
}

// TestReceiveSessionExpiresUnpaidInvoiceAtDeadline asserts an unpaid receive
// stops waiting for the mailbox event once the persisted invoice deadline
// elapses.
func TestReceiveSessionExpiresUnpaidInvoiceAtDeadline(t *testing.T) {
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
	client.outEvents = &blockingOutSwapEventReceiver{}
	useTestOnionDecoder(client, 42_000)

	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)
	deadline := time.Now().Add(-time.Second)
	require.NoError(
		t,
		session.mutateAndPersist(
			t.Context(),
			func() error {
				session.deadline = deadline

				return nil
			},
		),
	)
	// Keep the client's fake clock before the persisted deadline so the
	// test takes the invoice-deadline branch, while the real context
	// deadline is already elapsed. This avoids a millisecond-scale
	// wall-clock race.
	client.now = func() time.Time {
		return deadline.Add(-time.Second)
	}

	_, err = session.Wait(t.Context())
	require.ErrorIs(t, err, errSwapExpired)
	require.Equal(t, ReceiveStateExpired, session.State())

	resumed, err := client.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateExpired, resumed.State())

	pending, err := client.ListPendingReceiveSessions(t.Context())
	require.NoError(t, err)
	require.Empty(t, pending)
}

// TestReceiveSessionOverdueInvoiceAcceptsDeliveredEvent asserts an already
// delivered mailbox event wins over the invoice deadline during resume.
func TestReceiveSessionOverdueInvoiceAcceptsDeliveredEvent(t *testing.T) {
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
	client.overdueReceivePollWindow = 100 * time.Millisecond
	useTestOnionDecoder(client, 42_000)

	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)
	require.NoError(
		t,
		session.mutateAndPersist(
			t.Context(),
			func() error {
				session.deadline = time.Now().Add(-time.Second)

				return nil
			},
		),
	)

	resumed, err := client.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateInvoiceCreated, resumed.State())

	err = resumed.waitForHTLCEvent(t.Context())
	require.NoError(t, err)
	require.Equal(t, ReceiveStateHTLCEventAccepted, resumed.State())
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
	useTestOnionDecoder(client, 42_000)
	client.waitPollInterval = time.Millisecond

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
	require.Contains(
		t, resumed.TerminalReason(),
		"does not match invoice amount",
	)
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
			PkScript: []byte{
				0x51,
			},
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
		},
		sendSessionID: "claim-session",
	}

	client := NewSwapClient(serverConn, daemonConn, nil, creator)
	useTestOnionDecoder(client, 42_000)
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

// TestReceiveSessionClaimBeforeHTLCEventFailsClearly asserts manual claims
// cannot proceed before mailbox metadata identifies the concrete vHTLC.
func TestReceiveSessionClaimBeforeHTLCEventFailsClearly(t *testing.T) {
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
			PkScript: []byte{
				0x51,
			},
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
		},
		sendSessionID: "claim-session",
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	useTestOnionDecoder(client, 42_000)
	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)

	_, err = session.Claim(t.Context(), "funding:0", 42_000)
	require.ErrorContains(
		t, err, "before out-swap HTLC event is accepted",
	)
	require.Equal(t, ReceiveStateInvoiceCreated, session.State())
	require.Zero(t, daemonConn.sendCustomCalls)
}

// TestReceiveSessionClaimFailsOnAmountMismatch asserts manual claims use the
// same amount guard as automatic funding observation after the HTLC event is
// accepted.
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
	cfg := VHTLCConfig{
		RefundLocktime:                       144,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                24,
		UnilateralRefundWithoutReceiverDelay: 36,
		SwapServerPubkey:                     serverPubKey,
	}
	serverConn := &testSwapServerConn{
		hint: &RouteHint{
			NodeID:          serverPubKey,
			ChannelID:       99,
			FeeBaseMsat:     1,
			FeePropPpm:      2,
			CltvExpiryDelta: 40,
		},
		cfg: &cfg,
	}
	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
		blockHeight: 100,
		receiveInfo: &ReceiveInfo{
			PkScript: []byte{
				0x51,
			},
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
		},
		sendSessionID: "claim-session",
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, creator, store,
	)
	useTestOnionDecoder(client, 42_000)
	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)
	acceptTestOutSwapHtlcEvent(t, client, session, cfg)

	_, err = session.Claim(t.Context(), "funding:0", 41_999)
	require.ErrorContains(t, err, "does not match invoice amount")
	require.Equal(t, ReceiveStateFailed, session.State())
	require.Contains(
		t, session.TerminalReason(),
		"does not match invoice amount",
	)
	require.Empty(t, session.InterventionReason())
	require.Zero(t, daemonConn.sendCustomCalls)
}

// TestReceiveSessionFreshClaimBoundsSpentLookup asserts the first claim attempt
// does not spend the caller's whole context on optional duplicate-claim
// reconciliation.
func TestReceiveSessionFreshClaimBoundsSpentLookup(t *testing.T) {
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

	daemonConn := &testDaemonConn{
		blockHeight: 100,
		receiveInfo: &ReceiveInfo{
			PkScript: []byte{
				0x51,
			},
			PubKeyXOnly: receiverPriv.PubKey().X().Bytes(),
		},
		sendSessionID:  "claim-session",
		spentLookupErr: context.DeadlineExceeded,
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	session := &ReceiveSession{
		client:              client,
		state:               ReceiveStateVHTLCFunded,
		Preimage:            preimage,
		PaymentHash:         preimage.Hash(),
		clientPubKey:        receiverPriv.PubKey(),
		swapServerPubKey:    senderPriv.PubKey(),
		operatorPubKey:      operatorPriv.PubKey(),
		vhtlcPolicy:         policy,
		vhtlcPolicyTemplate: policyTemplate,
		vhtlcPkScript:       pkScript,
		vhtlcConfig: VHTLCConfig{
			RefundLocktime: 144,
		},
		vhtlcOutpoint: "funding:0",
		vhtlcAmount:   42_000,
	}

	_, err = session.Claim(t.Context(), "funding:0", 42_000)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateCompleted, session.State())
	require.Equal(t, 1, daemonConn.sendCustomCalls)
	require.Equal(t, 1, daemonConn.spentLookupCalls)
	require.Equal(t, 1, daemonConn.armRecoveryCalls)
	require.Equal(
		t, recoveryDirectionReceive, daemonConn.lastArmRecovery.
			GetDirection(),
	)
	require.Equal(
		t, recoveryActionClaim, daemonConn.lastArmRecovery.
			GetAction(),
	)
	require.Equal(
		t, "funding:0", daemonConn.lastArmRecovery.
			GetVtxoOutpoint(),
	)
	require.Equal(
		t, int32(144), daemonConn.lastArmRecovery.
			GetRefundLocktime(),
	)
	require.Equal(t, 1, daemonConn.cancelCalls)
}

// TestReceiveSessionFreshClaimBoundsSpentLookupGRPCDeadline mirrors
// TestReceiveSessionFreshClaimBoundsSpentLookup but the daemon returns the
// gRPC status form of DeadlineExceeded rather than context.DeadlineExceeded.
// The bounded reconcile must still swallow it so the bounded check stays
// agnostic to the wire encoding the caller's transport happens to use.
func TestReceiveSessionFreshClaimBoundsSpentLookupGRPCDeadline(t *testing.T) {
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

	grpcDeadline := status.Error(
		codes.DeadlineExceeded, context.DeadlineExceeded.Error(),
	)

	daemonConn := &testDaemonConn{
		blockHeight: 100,
		receiveInfo: &ReceiveInfo{
			PkScript: []byte{
				0x51,
			},
			PubKeyXOnly: receiverPriv.PubKey().X().Bytes(),
		},
		sendSessionID: "claim-session",
		spentLookupErr: fmt.Errorf(
			"get indexed vtxo by script: %w", grpcDeadline,
		),
		spentLookupBlock: 50 * time.Millisecond,
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	client.waitPollInterval = 5 * time.Millisecond

	session := &ReceiveSession{
		client:              client,
		state:               ReceiveStateVHTLCFunded,
		Preimage:            preimage,
		PaymentHash:         preimage.Hash(),
		clientPubKey:        receiverPriv.PubKey(),
		swapServerPubKey:    senderPriv.PubKey(),
		operatorPubKey:      operatorPriv.PubKey(),
		vhtlcPolicy:         policy,
		vhtlcPolicyTemplate: policyTemplate,
		vhtlcPkScript:       pkScript,
		vhtlcConfig: VHTLCConfig{
			RefundLocktime: 144,
		},
		vhtlcOutpoint: "funding:0",
		vhtlcAmount:   42_000,
	}

	_, err = session.Claim(t.Context(), "funding:0", 42_000)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateCompleted, session.State())
	require.Equal(t, 1, daemonConn.sendCustomCalls)
	require.Equal(t, 1, daemonConn.spentLookupCalls)
}

// TestReceiveSessionClaimRejectsAfterRefundLocktime asserts a late manual
// claim does not race the swap server's refund once the refund path is mature.
func TestReceiveSessionClaimRejectsAfterRefundLocktime(t *testing.T) {
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

	daemonConn := &testDaemonConn{
		blockHeight: 144,
		identityKey: receiverPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
		receiveInfo: &ReceiveInfo{
			PkScript: []byte{
				0x51,
			},
			PubKeyXOnly: receiverPriv.PubKey().X().Bytes(),
		},
		sendSessionID: "claim-session",
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	session := &ReceiveSession{
		client:              client,
		state:               ReceiveStateVHTLCFunded,
		Preimage:            preimage,
		PaymentHash:         preimage.Hash(),
		clientPubKey:        receiverPriv.PubKey(),
		swapServerPubKey:    senderPriv.PubKey(),
		operatorPubKey:      operatorPriv.PubKey(),
		vhtlcPolicy:         policy,
		vhtlcPolicyTemplate: policyTemplate,
		vhtlcPkScript:       pkScript,
		vhtlcConfig: VHTLCConfig{
			RefundLocktime: 144,
		},
		vhtlcOutpoint: "funding:0",
		vhtlcAmount:   42_000,
	}

	_, err = session.Claim(t.Context(), "funding:0", 42_000)
	require.ErrorIs(t, err, errSwapExpired)
	require.Equal(t, ReceiveStateExpired, session.State())
	require.Zero(t, daemonConn.sendCustomCalls)
}

// TestReceiveSessionClaimRecoveryCompletionWinsAfterRefundLocktime asserts
// daemon-owned recovery can complete a receive session even after the
// cooperative claim locktime gate would otherwise expire new claim attempts.
func TestReceiveSessionClaimRecoveryCompletionWinsAfterRefundLocktime(
	t *testing.T) {

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

	recoveryID := "claim-recovery"
	daemonConn := &testDaemonConn{
		blockHeight: 144,
		receiveInfo: &ReceiveInfo{
			PkScript: []byte{
				0x51,
			},
			PubKeyXOnly: receiverPriv.PubKey().X().Bytes(),
		},
		statusResp: &daemonrpc.GetVHTLCRecoveryStatusResponse{
			Found: true,
			Status: &daemonrpc.VHTLCRecoveryStatus{
				RecoveryId: recoveryID,
				State:      recoveryStateCompleted,
			},
		},
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	session := &ReceiveSession{
		client:              client,
		state:               ReceiveStateVHTLCFunded,
		Preimage:            preimage,
		PaymentHash:         preimage.Hash(),
		vhtlcPolicy:         policy,
		vhtlcPolicyTemplate: policyTemplate,
		vhtlcPkScript:       pkScript,
		vhtlcConfig: VHTLCConfig{
			RefundLocktime: 144,
		},
		vhtlcOutpoint:   "funding:0",
		vhtlcAmount:     42_000,
		claimRecoveryID: recoveryID,
	}

	result, err := session.Claim(t.Context(), "funding:0", 42_000)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateCompleted, session.State())
	require.Equal(t, "funding:0", result.VTXOOutpoint)
	require.Equal(t, recoveryID, daemonConn.lastStatus.GetRecoveryId())
	require.Equal(t, 1, daemonConn.statusCalls)
	require.Zero(t, daemonConn.sendCustomCalls)
}

// TestReceiveSessionClaimRejectsSpentVHTLCWithoutPreimage asserts the client
// does not submit a claim once the indexed vHTLC spend lacks the invoice
// preimage.
func TestReceiveSessionClaimRejectsSpentVHTLCWithoutPreimage(t *testing.T) {
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

	daemonConn := &testDaemonConn{
		blockHeight: 100,
		spentVTXO: &VTXOInfo{
			Outpoint:    "funding:0",
			AmountSat:   42_000,
			SpentByTxID: "refund-session",
		},
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	session := &ReceiveSession{
		client:              client,
		state:               ReceiveStateVHTLCFunded,
		Preimage:            preimage,
		PaymentHash:         preimage.Hash(),
		vhtlcPolicy:         policy,
		vhtlcPolicyTemplate: policyTemplate,
		vhtlcPkScript:       pkScript,
		vhtlcConfig: VHTLCConfig{
			RefundLocktime: 144,
		},
		vhtlcOutpoint: "funding:0",
		vhtlcAmount:   42_000,
	}

	_, err = session.Claim(t.Context(), "funding:0", 42_000)
	require.ErrorIs(t, err, errReceiveVHTLCSpentWithoutPreimage)
	require.Equal(t, ReceiveStateFailed, session.State())
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
	useTestOnionDecoder(client, 42_000)
	session, err := client.StartReceiveViaLightning(
		t.Context(), btcutil.Amount(42_000),
	)
	require.NoError(t, err)
	acceptTestOutSwapHtlcEvent(t, client, session, *serverConn.cfg)
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
			PkScript: []byte{
				0x51,
			},
			PubKeyXOnly: receiverPriv.PubKey().X().Bytes(),
		},
		sendCustomErr: sendErr,
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	client.claimMaxAttempts = 2
	client.claimRetryDelay = time.Nanosecond

	_, err = client.claimReceiveVHTLC(
		t.Context(), preimage.Hash(), preimage, policy, policyTemplate,
		pkScript, "funding:0", 42_000,
		receiverPriv.PubKey().X().Bytes(),
	)
	require.ErrorIs(t, err, sendErr)
	require.ErrorContains(t, err, "claim vHTLC")
	require.NotContains(t, err.Error(), "<nil>")
	require.Equal(t, 2, daemonConn.sendCustomCalls)
}
