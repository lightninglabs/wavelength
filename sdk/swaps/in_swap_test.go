package swaps

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	swapsqlc "github.com/lightninglabs/darepo-client/sdk/swaps/sqlc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// testInSwapAmountSat is the total vHTLC amount returned by the fake
	// swap server.
	testInSwapAmountSat = 42_000

	// testInSwapFeeSat is the swap server fee returned by the fake swap
	// server.
	testInSwapFeeSat = 123

	// testInSwapInvoiceSat is the original Lightning invoice amount.
	testInSwapInvoiceSat = testInSwapAmountSat - testInSwapFeeSat
)

type testInSwapServerConn struct {
	cfg                 *InSwapConfig
	quote               *InSwapQuote
	quoteErr            error
	quoteCalls          int
	quoteInvoice        string
	quoteMaxFeeSat      uint64
	createCalls         int
	createAccountKey    []byte
	createMaxCreditSat  uint64
	refundAuthorization *InSwapRefundAuthorization
	refundAuthorizeErr  error
	refundAuthorizeReq  *testRefundAuthorizeReq
}

type testRefundAuthorizeReq struct {
	paymentHash     lntypes.Hash
	vhtlcOutpoint   string
	vhtlcAmountSat  int64
	vhtlcPolicy     []byte
	refundSpendPath []byte
	checkpointPSBT  []byte
}

type failAtExecDBTX struct {
	db     *sql.DB
	err    error
	failAt int
	calls  int
}

func (f *failAtExecDBTX) ExecContext(ctx context.Context, query string,
	args ...interface{}) (sql.Result, error) {

	f.calls++
	if f.calls == f.failAt {
		return nil, f.err
	}

	return f.db.ExecContext(ctx, query, args...)
}

func (f *failAtExecDBTX) PrepareContext(ctx context.Context, query string) (
	*sql.Stmt, error) {

	return f.db.PrepareContext(ctx, query)
}

func (f *failAtExecDBTX) QueryContext(ctx context.Context, query string,
	args ...interface{}) (*sql.Rows, error) {

	return f.db.QueryContext(ctx, query, args...)
}

func (f *failAtExecDBTX) QueryRowContext(ctx context.Context, query string,
	args ...interface{}) *sql.Row {

	return f.db.QueryRowContext(ctx, query, args...)
}

// RequestChannelID is unused in these tests.
func (c *testInSwapServerConn) RequestChannelID(_ context.Context,
	_ *btcec.PublicKey, _ lntypes.Hash, _ btcutil.Amount, _ uint32) (
	*OutSwapQuote, error) {

	return nil, nil
}

// AcknowledgeOutSwapHTLC is unused in these tests.
func (c *testInSwapServerConn) AcknowledgeOutSwapHTLC(context.Context,
	lntypes.Hash, *btcec.PublicKey) error {

	return nil
}

// CreateInSwap returns the preconfigured in-swap config.
func (c *testInSwapServerConn) CreateInSwap(context.Context, string, uint64,
	*btcec.PublicKey) (*InSwapConfig, error) {

	c.createCalls++

	return c.cfg, nil
}

// CreateInSwapWithCredits returns the preconfigured in-swap config and records
// the credit account options passed by the client.
func (c *testInSwapServerConn) CreateInSwapWithCredits(_ context.Context,
	_ string, _ uint64, _ *btcec.PublicKey, accountKey []byte,
	maxCreditSat uint64) (*InSwapConfig, error) {

	c.createCalls++
	c.createAccountKey = append([]byte(nil), accountKey...)
	c.createMaxCreditSat = maxCreditSat

	return c.cfg, nil
}

// QuoteInSwap returns the preconfigured in-swap quote.
func (c *testInSwapServerConn) QuoteInSwap(_ context.Context, invoice string,
	maxFeeSat uint64) (*InSwapQuote, error) {

	c.quoteCalls++
	c.quoteInvoice = invoice
	c.quoteMaxFeeSat = maxFeeSat
	if c.quoteErr != nil {
		return nil, c.quoteErr
	}

	return c.quote, nil
}

// AuthorizeInSwapRefund records and returns the preconfigured refund
// authorization.
func (c *testInSwapServerConn) AuthorizeInSwapRefund(_ context.Context,
	paymentHash lntypes.Hash, vhtlcOutpoint string, vhtlcAmountSat int64,
	vhtlcPolicyTemplate, refundSpendPath, checkpointPSBT []byte) (
	*InSwapRefundAuthorization, error) {

	c.refundAuthorizeReq = &testRefundAuthorizeReq{
		paymentHash:     paymentHash,
		vhtlcOutpoint:   vhtlcOutpoint,
		vhtlcAmountSat:  vhtlcAmountSat,
		vhtlcPolicy:     append([]byte(nil), vhtlcPolicyTemplate...),
		refundSpendPath: append([]byte(nil), refundSpendPath...),
		checkpointPSBT:  append([]byte(nil), checkpointPSBT...),
	}
	if c.refundAuthorizeErr != nil {
		return nil, c.refundAuthorizeErr
	}
	if c.refundAuthorization != nil {
		return c.refundAuthorization, nil
	}

	return nil, status.Error(codes.FailedPrecondition, "refund unavailable")
}

// SignInSwapForfeit is unused in these tests.
func (c *testInSwapServerConn) SignInSwapForfeit(context.Context,
	*ForfeitSignaturePayload) (*ForfeitParticipantSignature, error) {

	return nil, errors.New("unexpected in-swap forfeit signature request")
}

// SubmitOutSwapForfeitSignature is unused in these tests.
func (c *testInSwapServerConn) SubmitOutSwapForfeitSignature(context.Context,
	*ForfeitSignaturePayload, *ForfeitParticipantSignature) error {

	return errors.New("unexpected out-swap forfeit signature submission")
}

// Close closes the server connection.
func (c *testInSwapServerConn) Close() error {
	return nil
}

// testPayInvoice creates a regtest BOLT-11 invoice for the supplied preimage
// and amount.
func testPayInvoice(t *testing.T, preimage lntypes.Preimage,
	amountSat btcutil.Amount) string {

	t.Helper()

	invoiceKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	creator := NewEphemeralInvoiceGenerator(
		invoiceKey, nil, &chaincfg.RegressionNetParams,
	)

	invoice, hash, err := creator.CreateInvoice(
		t.Context(), amountSat, "pay", &RouteHint{
			NodeID: invoiceKey.
				PubKey().
				SerializeCompressed(),
			ChannelID:       1,
			CltvExpiryDelta: 40,
		},
		time.Hour, &preimage,
	)
	require.NoError(t, err)
	require.Equal(t, preimage.Hash(), hash)

	return string(invoice.PaymentRequest)
}

// testValidPayInvoice creates the standard pay-side test invoice.
func testValidPayInvoice(t *testing.T, preimage lntypes.Preimage) string {
	t.Helper()

	return testPayInvoice(
		t, preimage, btcutil.Amount(testInSwapInvoiceSat),
	)
}

// testAmountlessPayInvoice creates a valid regtest BOLT-11 invoice without an
// amount.
func testAmountlessPayInvoice(t *testing.T, preimage lntypes.Preimage) string {
	t.Helper()

	invoiceKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	invoice, err := zpay32.NewInvoice(
		&chaincfg.RegressionNetParams, preimage.Hash(), time.Now(),
		zpay32.Description("pay"),
	)
	require.NoError(t, err)

	paymentRequest, err := invoice.Encode(zpay32.MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			return ecdsa.SignCompact(invoiceKey, msg, true), nil
		},
	})
	require.NoError(t, err)

	return paymentRequest
}

// configureTestPayClient sets the regtest invoice network on a test client.
func configureTestPayClient(client *SwapClient) *SwapClient {
	client.SetChainParams(&chaincfg.RegressionNetParams)

	return client
}

// testInSwapConfig returns a valid fake in-swap quote for the standard pay
// invoice used by the tests.
func testInSwapConfig(serverPubkey *btcec.PublicKey, preimage lntypes.Preimage,
	expiry time.Time) *InSwapConfig {

	return &InSwapConfig{
		PaymentHash:  preimage.Hash(),
		AmountSat:    testInSwapAmountSat,
		FeeSat:       testInSwapFeeSat,
		ServerPubkey: serverPubkey,
		VHTLCConfig: VHTLCConfig{
			RefundLocktime:                       144,
			UnilateralClaimDelay:                 12,
			UnilateralRefundDelay:                24,
			UnilateralRefundWithoutReceiverDelay: 36,
		},
		Expiry: expiry,
	}
}

// cloneInSwapConfig returns a shallow copy of cfg for validation tests that
// mutate one quoted field at a time.
func cloneInSwapConfig(cfg *InSwapConfig) *InSwapConfig {
	clone := *cfg

	return &clone
}

// TestQuotePayViaLightningReturnsBoundPreview verifies the non-mutating quote
// path validates the server preview without creating an in-swap.
func TestQuotePayViaLightningReturnsBoundPreview(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	expiry := time.Now().Add(time.Minute)
	serverConn := &testInSwapServerConn{
		quote: &InSwapQuote{
			PaymentHash:      preimage.Hash(),
			InvoiceAmountSat: testInSwapInvoiceSat,
			AmountSat:        testInSwapAmountSat,
			FeeSat:           testInSwapFeeSat,
			Expiry:           expiry,
			SettlementType:   SettlementTypeLightning,
			ExceedsMaxFee:    true,
		},
	}
	client := configureTestPayClient(
		NewSwapClient(serverConn, nil, nil, nil),
	)

	quote, err := client.QuotePayViaLightning(
		t.Context(), invoice, testInSwapFeeSat-1,
	)
	require.NoError(t, err)
	require.Equal(t, preimage.Hash(), quote.PaymentHash)
	require.Equal(t, uint64(testInSwapInvoiceSat),
		quote.InvoiceAmountSat)
	require.Equal(t, uint64(testInSwapAmountSat), quote.AmountSat)
	require.Equal(t, uint64(testInSwapFeeSat), quote.FeeSat)
	require.True(t, quote.ExceedsMaxFee)
	require.Equal(t, SettlementTypeLightning, quote.SettlementType)
	require.Equal(t, 1, serverConn.quoteCalls)
	require.Equal(t, invoice, serverConn.quoteInvoice)
	require.Equal(t, uint64(testInSwapFeeSat-1),
		serverConn.quoteMaxFeeSat)
	require.Zero(t, serverConn.createCalls)
}

// TestValidateInSwapPreviewAllowsCreditOnlyZeroAmount verifies credit-only
// quotes can report zero Ark funding while still binding to the invoice.
func TestValidateInSwapPreviewAllowsCreditOnlyZeroAmount(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	quote := &InSwapQuote{
		PaymentHash:      preimage.Hash(),
		InvoiceAmountSat: testInSwapInvoiceSat,
		AmountSat:        0,
		FeeSat:           0,
		Expiry:           time.Now().Add(time.Minute),
		SettlementType:   SettlementTypeCredit,
		CreditQuote: &CreditQuote{
			MustUseCredit:    true,
			CreditAppliedSat: uint64(testInSwapInvoiceSat),
		},
	}

	err = validateInSwapPreview(
		invoice, quote, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
}

// TestValidateInSwapQuoteRejectsServerMismatches verifies the client treats
// the swap server response as a quote that must match the caller's invoice.
func TestValidateInSwapQuoteRejectsServerMismatches(t *testing.T) {
	t.Parallel()

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	cfg := testInSwapConfig(
		serverPriv.PubKey(), preimage, time.Now().Add(time.Minute),
	)
	err = validateInSwapQuote(
		invoice, testInSwapFeeSat, cfg, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	otherPreimage, err := NewPreimage()
	require.NoError(t, err)

	tests := []struct {
		name        string
		invoice     string
		maxFeeSat   uint64
		chainParams *chaincfg.Params
		mutate      func(*InSwapConfig)
		wantErr     string
	}{{
		name:        "missing chain params",
		invoice:     invoice,
		maxFeeSat:   testInSwapFeeSat,
		chainParams: nil,
		wantErr:     "chain params",
	}, {
		name:        "wrong network",
		invoice:     invoice,
		maxFeeSat:   testInSwapFeeSat,
		chainParams: &chaincfg.MainNetParams,
		wantErr:     "decode invoice",
	}, {
		name:        "payment hash mismatch",
		invoice:     invoice,
		maxFeeSat:   testInSwapFeeSat,
		chainParams: &chaincfg.RegressionNetParams,
		mutate: func(cfg *InSwapConfig) {
			cfg.PaymentHash = otherPreimage.Hash()
		},
		wantErr: "payment hash does not match",
	}, {
		name:        "fee above caller limit",
		invoice:     invoice,
		maxFeeSat:   testInSwapFeeSat - 1,
		chainParams: &chaincfg.RegressionNetParams,
		wantErr:     "exceeds max fee",
	}, {
		name:        "fee overflows int64",
		invoice:     invoice,
		maxFeeSat:   ^uint64(0),
		chainParams: &chaincfg.RegressionNetParams,
		mutate: func(cfg *InSwapConfig) {
			cfg.FeeSat = maxInt64Uint + 1
		},
		wantErr: "fee overflows int64 range",
	}, {
		name:        "amount below invoice plus fee",
		invoice:     invoice,
		maxFeeSat:   testInSwapFeeSat,
		chainParams: &chaincfg.RegressionNetParams,
		mutate: func(cfg *InSwapConfig) {
			cfg.AmountSat--
		},
		wantErr: "does not equal invoice amount",
	}, {
		name:        "amount above invoice plus fee",
		invoice:     invoice,
		maxFeeSat:   testInSwapFeeSat,
		chainParams: &chaincfg.RegressionNetParams,
		mutate: func(cfg *InSwapConfig) {
			cfg.AmountSat++
		},
		wantErr: "does not equal invoice amount",
	}}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := cloneInSwapConfig(cfg)
			if test.mutate != nil {
				test.mutate(cfg)
			}

			err := validateInSwapQuote(
				test.invoice, test.maxFeeSat, cfg,
				test.chainParams,
			)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

// TestValidateInSwapQuoteRejectsAmountlessInvoice verifies pay swaps require a
// fixed BOLT-11 amount because the client cannot safely infer it from the
// server quote.
func TestValidateInSwapQuoteRejectsAmountlessInvoice(t *testing.T) {
	t.Parallel()

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testAmountlessPayInvoice(t, preimage)

	cfg := testInSwapConfig(
		serverPriv.PubKey(), preimage, time.Now().Add(time.Minute),
	)
	err = validateInSwapQuote(
		invoice, testInSwapFeeSat, cfg, &chaincfg.RegressionNetParams,
	)
	require.ErrorContains(t, err, "invoice amount")
}

// TestPaySessionRejectsUnboundInSwapQuote verifies quote validation runs
// before the client funds the server-returned vHTLC.
func TestPaySessionRejectsUnboundInSwapQuote(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	otherPreimage, err := NewPreimage()
	require.NoError(t, err)

	cfg := testInSwapConfig(
		serverPriv.PubKey(), otherPreimage, time.Now().Add(time.Minute),
	)
	serverConn := &testInSwapServerConn{
		cfg: cfg,
	}

	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
	}

	client := configureTestPayClient(
		NewSwapClient(serverConn, daemonConn, nil, nil),
	)

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.Nil(t, session)
	require.ErrorContains(t, err, "payment hash does not match")
	require.Zero(t, daemonConn.sendPolicyCalls)
}

// TestPaySessionCreditOnlyStartCompletesWithoutVHTLC verifies credit-only
// pays can complete during creation without forcing the Loop FSM through a
// vHTLC funding state.
func TestPaySessionCreditOnlyStartCompletesWithoutVHTLC(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)
	creditSat := uint64(testInSwapInvoiceSat)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:    preimage.Hash(),
			AmountSat:      0,
			FeeSat:         0,
			SettlementType: SettlementTypeCredit,
			CreditQuote: &CreditQuote{
				MustUseCredit:    true,
				CreditAppliedSat: creditSat,
			},
			Preimage: &preimage,
			Expiry:   time.Now().Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
	}

	client := configureTestPayClient(
		NewSwapClient(serverConn, daemonConn, nil, nil),
	)

	session, err := client.StartPayViaLightningWithCredits(
		t.Context(), invoice, testInSwapFeeSat, creditSat,
	)
	require.NoError(t, err)
	require.Equal(t, PayStateCompleted, session.State())
	require.Zero(t, daemonConn.sendPolicyCalls)
	require.Equal(t, 1, serverConn.createCalls)
	require.Equal(
		t, clientPriv.PubKey().SerializeCompressed(),
		serverConn.createAccountKey,
	)
	require.Equal(t, creditSat, serverConn.createMaxCreditSat)

	result, err := session.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, preimage.Hash(), result.PaymentHash)
	require.Equal(t, preimage, result.Preimage)
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
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
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
		spentVTXO: &VTXOInfo{
			SpentByTxID: "0123456789abcdef0123456789abcdef" +
				"0123456789abcdef0123456789abcdef",
		},
		indexedPackage: &OORPackageInfo{
			CheckpointPSBTs: [][]byte{
				testCheckpointPSBTWithPreimage(
					t, preimage[:],
				),
			},
		},
	}

	client := configureTestPayClient(
		NewSwapClient(serverConn, daemonConn, nil, nil),
	)
	client.waitPollInterval = time.Millisecond
	client.fundingExpiryBuffer = 0

	result, err := client.PayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)
	require.Equal(t, preimage.Hash(), result.PaymentHash)
	require.Equal(t, preimage, result.Preimage)
	require.Equal(t, "funding-session", result.FundingSessionID)
	require.EqualValues(t, testInSwapFeeSat, result.FeeSat)
	require.NotEmpty(t, daemonConn.lastSendPolicy)
}

// TestPayViaLightningRequiresClaimPreimage asserts the pay FSM never treats an
// absent live vHTLC as completion unless the claim preimage is actually
// indexed.
func TestPayViaLightningRequiresClaimPreimage(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       144,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(200 * time.Millisecond),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		sendSessionID: "funding-session",
	}

	client := configureTestPayClient(
		NewSwapClient(serverConn, daemonConn, nil, nil),
	)
	client.waitPollInterval = time.Millisecond

	result, err := client.PayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.ErrorIs(t, err, errSwapExpired)
	require.Nil(t, result)
}

// TestPaySessionRefundsFundedVHTLCOnTimeout asserts a pay session
// automatically sweeps its funded vHTLC back through the sender refund path
// once the server claim deadline elapses.
func TestPaySessionRefundsFundedVHTLCOnTimeout(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	refundScript, err := txscript.PayToTaprootScript(clientPriv.PubKey())
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	now := time.Unix(1_700_000_000, 0)
	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       100,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: now.Add(time.Millisecond),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		blockHeight:   100,
		sendSessionID: "refund-session",
		vhtlc: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: testInSwapAmountSat,
		},
		receiveInfo: &ReceiveInfo{
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
			PkScript:    refundScript,
		},
		spendOnCustom: true,
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond
	client.refundLocktimeBuffer = 0
	client.now = func() time.Time { return now }

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	client.now = func() time.Time {
		return now.Add(2 * time.Millisecond)
	}

	_, err = session.Wait(t.Context())
	require.ErrorIs(t, err, ErrSwapRefunded)
	require.Equal(t, 1, daemonConn.sendCustomCalls)
	require.NotEmpty(t, daemonConn.lastClaimInput[0].SpendPath)

	resumed, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateRefunded, resumed.State())
	require.Equal(t, "refund-session", resumed.refundSessionID)
}

// TestPaySessionCooperativeRefundsBeforeTimeout asserts a pay session can
// sweep a funded vHTLC immediately once the swap server has safely failed the
// Lightning payment and signs the exact prepared refund checkpoint.
func TestPaySessionCooperativeRefundsBeforeTimeout(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	refundScript, err := txscript.PayToTaprootScript(clientPriv.PubKey())
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       100,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
		refundAuthorization: &InSwapRefundAuthorization{
			Signature: TaprootScriptSignature{
				PubKey: serverPriv.
					PubKey().
					SerializeCompressed(),
				WitnessScript: []byte("witness"),
				Signature:     []byte("server-sig"),
			},
			FailureReason: "payment failed",
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		blockHeight:   50,
		sendSessionID: "refund-session",
		vhtlc: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: testInSwapAmountSat,
		},
		receiveInfo: &ReceiveInfo{
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
			PkScript:    refundScript,
		},
		spendOnCustom: true,
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond
	client.refundLocktimeBuffer = 0

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	_, err = session.Wait(t.Context())
	require.ErrorIs(t, err, ErrSwapRefunded)
	require.Equal(t, 1, daemonConn.sendCustomCalls)
	require.NotNil(t, serverConn.refundAuthorizeReq)
	require.Equal(
		t, "funding:0", serverConn.refundAuthorizeReq.vhtlcOutpoint,
	)
	require.EqualValues(
		t, testInSwapAmountSat,
		serverConn.refundAuthorizeReq.vhtlcAmountSat,
	)
	require.Equal(
		t, []byte("checkpoint"),
		serverConn.refundAuthorizeReq.checkpointPSBT,
	)
	require.Len(t, daemonConn.lastClaimInput, 1)
	require.Len(t, daemonConn.lastClaimInput[0].ExternalSignatures, 1)
	require.Equal(
		t, []byte("server-sig"),
		daemonConn.lastClaimInput[0].ExternalSignatures[0].Signature,
	)

	resumed, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateRefunded, resumed.State())
	require.Equal(t, "refund-session", resumed.refundSessionID)
}

// TestPaySessionCooperativeRefundUsesLocalFundingMetadata asserts the refund
// path can proceed from the persisted funding response when the authoritative
// indexer rejects the vHTLC script lookup for the client's principal.
func TestPaySessionCooperativeRefundUsesLocalFundingMetadata(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	refundScript, err := txscript.PayToTaprootScript(clientPriv.PubKey())
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       100,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
		refundAuthorization: &InSwapRefundAuthorization{
			Signature: TaprootScriptSignature{
				PubKey: serverPriv.
					PubKey().
					SerializeCompressed(),
				WitnessScript: []byte("witness"),
				Signature:     []byte("server-sig"),
			},
			FailureReason: "payment failed",
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		blockHeight:   50,
		sendSessionID: "refund-session",
		sendOutpoint:  "funding:0",
		receiveInfo: &ReceiveInfo{
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
			PkScript:    refundScript,
		},
		spendOnCustom: true,
	}
	daemonConn.liveLookupHook = func(call int) (*VTXOInfo, error) {
		if call <= 2 {
			return nil, unregisteredScriptIndexerErr()
		}

		scriptKey := hex.EncodeToString(refundScript)
		if vtxo := daemonConn.liveByPkScript[scriptKey]; vtxo != nil {
			return vtxo, nil
		}

		return nil, nil
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond
	client.refundLocktimeBuffer = 0

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	_, err = session.Wait(t.Context())
	require.ErrorIs(t, err, ErrSwapRefunded)
	require.Equal(t, 1, daemonConn.sendPolicyCalls)
	require.GreaterOrEqual(t, daemonConn.liveLookupCalls, 1)
	require.Equal(t, 1, daemonConn.sendCustomCalls)
	require.NotNil(t, serverConn.refundAuthorizeReq)
	require.Equal(
		t, "funding:0", serverConn.refundAuthorizeReq.vhtlcOutpoint,
	)
	require.EqualValues(
		t, testInSwapAmountSat,
		serverConn.refundAuthorizeReq.vhtlcAmountSat,
	)
	require.Equal(t, "funding:0", session.vhtlcOutpoint)
	require.EqualValues(t, testInSwapAmountSat, session.vhtlcAmount)
}

// TestPaySessionCooperativeRefundWaitsForOutputBeforeTerminal verifies the
// client does not make a refund-accepted swap terminal before wallet recovery
// is visible. A caller retrying the same invoice should still see this swap as
// pending until the refund output or spent vHTLC is indexed.
func TestPaySessionCooperativeRefundWaitsForOutputBeforeTerminal(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	refundScript, err := txscript.PayToTaprootScript(clientPriv.PubKey())
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       100,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
		refundAuthorization: &InSwapRefundAuthorization{
			Signature: TaprootScriptSignature{
				PubKey: serverPriv.
					PubKey().
					SerializeCompressed(),
				WitnessScript: []byte("witness"),
				Signature:     []byte("server-sig"),
			},
			FailureReason: "payment failed",
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		blockHeight:   50,
		sendSessionID: "refund-session",
		sendOutpoint:  "funding:0",
		receiveInfo: &ReceiveInfo{
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
			PkScript:    refundScript,
		},
	}
	daemonConn.liveLookupHook = func(call int) (*VTXOInfo, error) {
		if daemonConn.sendCustomCalls == 0 {
			return nil, unregisteredScriptIndexerErr()
		}

		return nil, nil
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = 5 * time.Millisecond
	client.refundLocktimeBuffer = 0

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	err = session.runUntil(t.Context(), PayStateWaitingForClaim)
	require.NoError(t, err)
	require.Equal(t, PayStateWaitingForClaim, session.State())

	refunded, err := session.tryCooperativeRefund(t.Context())
	require.NoError(t, err)
	require.True(t, refunded)
	require.Equal(t, PayStateRefundInitiated, session.State())
	require.Equal(t, "refund-session", session.refundSessionID)
	require.Equal(t, 1, daemonConn.sendCustomCalls)
	require.NotNil(t, serverConn.refundAuthorizeReq)

	waitCtx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	err = session.completeRefund(waitCtx)
	require.NoError(t, err)
	require.Equal(t, PayStateRefundInitiated, session.State())
	require.Equal(t, 1, daemonConn.oorSessionCalls)
	require.Equal(t, "refund-session", daemonConn.lastOORSessionID)

	daemonConn.oorSession = &daemonrpc.OORSessionInfo{
		SessionId: "refund-session",
		Status: daemonrpc.
			OORSessionStatus_OOR_SESSION_STATUS_COMPLETED,
		Phase: "completed",
	}

	err = session.completeRefund(t.Context())
	require.NoError(t, err)
	require.Equal(t, PayStateRefunded, session.State())
	require.Equal(t, 1, daemonConn.cancelCalls)
	require.Equal(
		t, recoveryReasonRefundSessionCompleted,
		daemonConn.lastCancel.GetReason(),
	)
}

// TestPaySessionCooperativeRefundIgnoresServerUnavailable asserts a pay
// session keeps waiting when refund authorization is not available from the
// swap server, such as while the swap daemon is restarting or when talking to
// an older server that does not implement the RPC.
func TestPaySessionCooperativeRefundIgnoresServerUnavailable(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	refundScript, err := txscript.PayToTaprootScript(clientPriv.PubKey())
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       100,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
		blockHeight: 50,
		vhtlc: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: testInSwapAmountSat,
		},
		receiveInfo: &ReceiveInfo{
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
			PkScript:    refundScript,
		},
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond
	client.refundLocktimeBuffer = 0

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	err = session.runUntil(t.Context(), PayStateWaitingForClaim)
	require.NoError(t, err)

	for _, code := range []codes.Code{
		codes.Unavailable,
		codes.Unimplemented,
	} {
		serverConn.refundAuthorizeErr = status.Error(
			code, "refund authorization unavailable",
		)

		refunded, err := session.tryCooperativeRefund(t.Context())
		require.NoError(t, err)
		require.False(t, refunded)
		require.Equal(t, PayStateWaitingForClaim, session.State())
		require.Zero(t, daemonConn.sendCustomCalls)
	}
}

// TestPaySessionCooperativeRefundKeepsAcceptedSessionOnPersistFailure asserts
// the accepted daemon OOR session id stays in memory if the following swap DB
// write fails. The next tick must advance from the remembered accepted session
// instead of submitting a second custom-input refund spend.
func TestPaySessionCooperativeRefundKeepsAcceptedSessionOnPersistFailure(
	t *testing.T) {

	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	refundScript, err := txscript.PayToTaprootScript(clientPriv.PubKey())
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       100,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
		refundAuthorization: &InSwapRefundAuthorization{
			Signature: TaprootScriptSignature{
				PubKey: serverPriv.
					PubKey().
					SerializeCompressed(),
				WitnessScript: []byte("witness"),
				Signature:     []byte("server-sig"),
			},
			FailureReason: "payment failed",
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		blockHeight:   50,
		sendSessionID: "refund-session",
		vhtlc: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: testInSwapAmountSat,
		},
		receiveInfo: &ReceiveInfo{
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
			PkScript:    refundScript,
		},
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond
	client.refundLocktimeBuffer = 0

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	err = session.runUntil(t.Context(), PayStateWaitingForClaim)
	require.NoError(t, err)

	_, err = session.refundPubKey(t.Context())
	require.NoError(t, err)
	funded, err := session.observeRefundableVHTLC(t.Context())
	require.NoError(t, err)
	require.True(t, funded)

	persistErr := errors.New("persist failed")
	failingDB := &failAtExecDBTX{
		db:     store.db,
		err:    persistErr,
		failAt: 2,
	}
	store.queries = swapsqlc.New(failingDB)

	refunded, err := session.tryCooperativeRefund(t.Context())
	require.ErrorContains(t, err, "persist failed")
	require.False(t, refunded)
	require.Equal(t, "refund-session", session.refundSessionID)
	require.Equal(t, "payment failed", session.interventionReason)
	require.Equal(t, PayStateWaitingForClaim, session.State())
	require.Equal(t, 1, daemonConn.sendCustomCalls)
	require.Equal(t, 2, failingDB.calls)

	refunded, err = session.tryCooperativeRefund(t.Context())
	require.NoError(t, err)
	require.True(t, refunded)
	require.Equal(t, "refund-session", session.refundSessionID)
	require.Equal(t, PayStateRefundInitiated, session.State())
	require.Equal(t, 1, daemonConn.sendCustomCalls)
}

// TestPaySessionResumeAfterAcceptedRefundFindsOutput exercises the crash window
// after the daemon has accepted a cooperative refund OOR but before the swap DB
// write records the refund session id. A restarted client in that window
// reloads the pay session with an empty refundSessionID, while the
// wallet/indexer may already report the vHTLC as spent by the accepted refund.
// The important invariant is that this spent-without-preimage observation must
// not force NeedsIntervention if the persisted refund destination output is
// live; instead the client should reconcile the refund as terminally recovered
// and avoid submitting a second custom-input spend.
func TestPaySessionResumeAfterAcceptedRefundFindsOutput(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	refundScript, err := txscript.PayToTaprootScript(clientPriv.PubKey())
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       100,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
		refundAuthorization: &InSwapRefundAuthorization{
			Signature: TaprootScriptSignature{
				PubKey: serverPriv.
					PubKey().
					SerializeCompressed(),
				WitnessScript: []byte("witness"),
				Signature:     []byte("server-sig"),
			},
			FailureReason: "payment failed",
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		blockHeight:   50,
		sendSessionID: "refund-session",
		vhtlc: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: testInSwapAmountSat,
		},
		receiveInfo: &ReceiveInfo{
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
			PkScript:    refundScript,
		},
		spendOnCustom: true,
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond
	client.refundLocktimeBuffer = 0

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	err = session.runUntil(t.Context(), PayStateWaitingForClaim)
	require.NoError(t, err)

	_, err = session.refundPubKey(t.Context())
	require.NoError(t, err)
	funded, err := session.observeRefundableVHTLC(t.Context())
	require.NoError(t, err)
	require.True(t, funded)

	persistErr := errors.New("persist failed")
	failingDB := &failAtExecDBTX{
		db:     store.db,
		err:    persistErr,
		failAt: 2,
	}
	store.queries = swapsqlc.New(failingDB)

	refunded, err := session.tryCooperativeRefund(t.Context())
	require.ErrorContains(t, err, "persist failed")
	require.False(t, refunded)
	require.Equal(t, 1, daemonConn.sendCustomCalls)
	require.Equal(t, PayStateWaitingForClaim, session.State())

	daemonConn.liveByPkScript = map[string]*VTXOInfo{
		hex.EncodeToString(refundScript): {
			Outpoint:  daemonConn.sendSessionID + ":0",
			AmountSat: testInSwapAmountSat,
			PkScript:  refundScript,
		},
	}

	store.queries = swapsqlc.New(store.db)
	resumedClient := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	resumedClient.waitPollInterval = time.Millisecond
	resumedClient.refundLocktimeBuffer = 0

	resumed, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateWaitingForClaim, resumed.State())
	require.Empty(t, resumed.refundSessionID)
	require.NotEmpty(t, resumed.refundReceiveScript)

	refundOutput, err := resumed.observeRefundOutput(t.Context())
	require.NoError(t, err)
	require.NotNil(t, refundOutput)

	_, err = resumed.Wait(t.Context())
	require.ErrorIs(t, err, ErrSwapRefunded)
	require.Equal(t, PayStateRefunded, resumed.State())
	require.Equal(t, 1, daemonConn.sendCustomCalls)

	reloaded, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateRefunded, reloaded.State())
}

// TestPaySessionResumeAfterAcceptedRefundUsesSpentVHTLCFallback exercises the
// same accepted-refund restart window as
// TestPaySessionResumeAfterAcceptedRefundFindsOutput, but with the wallet
// indexer lagging on the newly-created refund output. In that case the funded
// vHTLC spend is already visible without a claim preimage, while the refund
// destination script returns no live output yet. The session must still treat
// the spend as a successful cooperative refund, because the pre-locktime refund
// path requires the client, operator, and server signatures. This pins the
// fallback that prevents a recovered refund from being misclassified as
// NeedsIntervention during restart reconciliation.
func TestPaySessionResumeAfterAcceptedRefundUsesSpentVHTLCFallback(
	t *testing.T) {

	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	refundScript, err := txscript.PayToTaprootScript(clientPriv.PubKey())
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       100,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
		refundAuthorization: &InSwapRefundAuthorization{
			Signature: TaprootScriptSignature{
				PubKey: serverPriv.
					PubKey().
					SerializeCompressed(),
				WitnessScript: []byte("witness"),
				Signature:     []byte("server-sig"),
			},
			FailureReason: "payment failed",
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		blockHeight:   50,
		sendSessionID: "refund-session",
		vhtlc: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: testInSwapAmountSat,
		},
		receiveInfo: &ReceiveInfo{
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
			PkScript:    refundScript,
		},
		spendOnCustom: true,
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond
	client.refundLocktimeBuffer = 0

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	err = session.runUntil(t.Context(), PayStateWaitingForClaim)
	require.NoError(t, err)

	_, err = session.refundPubKey(t.Context())
	require.NoError(t, err)
	funded, err := session.observeRefundableVHTLC(t.Context())
	require.NoError(t, err)
	require.True(t, funded)

	persistErr := errors.New("persist failed")
	failingDB := &failAtExecDBTX{
		db:     store.db,
		err:    persistErr,
		failAt: 2,
	}
	store.queries = swapsqlc.New(failingDB)

	refunded, err := session.tryCooperativeRefund(t.Context())
	require.ErrorContains(t, err, "persist failed")
	require.False(t, refunded)
	require.Equal(t, 1, daemonConn.sendCustomCalls)
	require.Equal(t, PayStateWaitingForClaim, session.State())

	daemonConn.liveByPkScript = nil

	store.queries = swapsqlc.New(store.db)
	resumedClient := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	resumedClient.waitPollInterval = time.Millisecond
	resumedClient.refundLocktimeBuffer = 0

	resumed, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateWaitingForClaim, resumed.State())
	require.Empty(t, resumed.refundSessionID)
	require.NotEmpty(t, resumed.refundReceiveScript)

	refundOutput, err := resumed.observeRefundOutput(t.Context())
	require.NoError(t, err)
	require.Nil(t, refundOutput)

	_, err = resumed.Wait(t.Context())
	require.ErrorIs(t, err, ErrSwapRefunded)
	require.Equal(t, PayStateRefunded, resumed.State())
	require.Equal(t, "refund-session", resumed.refundSessionID)
	require.Equal(t, 1, daemonConn.sendCustomCalls)

	reloaded, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateRefunded, reloaded.State())
}

// TestPaySessionObserveRefundOutputIgnoresWrongAmount verifies that a live VTXO
// at the persisted refund destination script is treated as "not our refund
// output" when its amount does not match the funded vHTLC amount. The lookup
// can race unrelated wallet state or future same-script activity. Retrying a
// permanent amount-mismatch error would wedge the pay FSM forever, while
// returning nil lets the caller keep reconciling through the spent-vHTLC
// fallback or normal polling instead.
func TestPaySessionObserveRefundOutputIgnoresWrongAmount(t *testing.T) {
	t.Parallel()

	refundScript := []byte{0x51, 0x20, 0x99}
	daemonConn := &testDaemonConn{
		liveByPkScript: map[string]*VTXOInfo{
			hex.EncodeToString(refundScript): {
				Outpoint:  "refund:0",
				AmountSat: testInSwapAmountSat - 1,
				PkScript:  refundScript,
			},
		},
	}
	preimage, err := NewPreimage()
	require.NoError(t, err)

	session := &paySession{
		client: configureTestPayClient(
			NewSwapClient(nil, daemonConn, nil, nil),
		),
		cfg: &InSwapConfig{
			PaymentHash: preimage.Hash(),
		},
		refundReceiveScript: refundScript,
		vhtlcOutpoint:       "funding:0",
		vhtlcAmount:         testInSwapAmountSat,
	}

	refundOutput, err := session.observeRefundOutput(t.Context())
	require.NoError(t, err)
	require.Nil(t, refundOutput)
	require.Equal(t, 1, daemonConn.liveLookupCalls)
}

// TestPaySessionRefundLocktimePollInterval documents the timeout-refund polling
// schedule used while a vHTLC is known but the refund branch is not yet
// spendable. Far from the refund locktime, the client should reduce noisy
// wallet and indexer polling by backing off from the normal SDK interval. Near
// the locktime, and once the locktime is reached, the helper must return the
// normal poll interval so the client remains responsive around the first height
// where a timeout refund can be submitted.
func TestPaySessionRefundLocktimePollInterval(t *testing.T) {
	t.Parallel()

	session := &paySession{
		client: &SwapClient{
			waitPollInterval: 2 * time.Second,
		},
		cfg: &InSwapConfig{
			VHTLCConfig: VHTLCConfig{
				RefundLocktime: 200,
			},
		},
	}

	require.Equal(
		t, time.Minute, session.refundLocktimePollInterval(50),
	)
	require.Equal(
		t, 20*time.Second, session.refundLocktimePollInterval(180),
	)
	require.Equal(
		t, 2*time.Second, session.refundLocktimePollInterval(197),
	)
	require.Equal(
		t, 2*time.Second, session.refundLocktimePollInterval(200),
	)
}

// TestPaySessionRefundsWhenRefundLocktimePassesBeforeClaim asserts that a
// funded pay session does not wait for the wall-clock swap deadline after the
// Ark refund locktime matures. The client should durably enter the refund path
// and sweep the vHTLC back as soon as the timeout branch is spendable.
func TestPaySessionRefundsWhenRefundLocktimePassesBeforeClaim(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	refundScript, err := txscript.PayToTaprootScript(clientPriv.PubKey())
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       100,
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
		blockHeight:   99,
		sendSessionID: "refund-session",
		vhtlc: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: testInSwapAmountSat,
		},
		receiveInfo: &ReceiveInfo{
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
			PkScript:    refundScript,
		},
		spendOnCustom: true,
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond
	client.refundLocktimeBuffer = 0

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	err = session.runUntil(t.Context(), PayStateWaitingForClaim)
	require.NoError(t, err)
	require.Equal(t, PayStateWaitingForClaim, session.State())

	daemonConn.blockHeight = 100

	_, err = session.Wait(t.Context())
	require.ErrorIs(t, err, ErrSwapRefunded)
	require.Equal(t, 1, daemonConn.sendCustomCalls)
	require.Equal(t, 1, daemonConn.armRecoveryCalls)
	require.Equal(
		t, recoveryDirectionPay, daemonConn.lastArmRecovery.
			GetDirection(),
	)
	require.Equal(
		t, recoveryActionRefundWithoutReceiver,
		daemonConn.lastArmRecovery.GetAction(),
	)
	require.Equal(
		t, "funding:0", daemonConn.lastArmRecovery.
			GetVtxoOutpoint(),
	)
	require.Equal(
		t, int32(100), daemonConn.lastArmRecovery.
			GetRefundLocktime(),
	)
	require.Equal(
		t, int32(36), daemonConn.lastArmRecovery.
			GetUnilateralRefundWithoutReceiverDelay(),
	)
	require.Equal(t, 1, daemonConn.cancelCalls)

	resumed, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateRefunded, resumed.State())
	require.Equal(t, "refund-session", resumed.refundSessionID)
}

// TestPaySessionResumeFromStore asserts the SDK can reload a persisted pay
// session from the isolated swap database and finish once the claim preimage is
// indexed.
func TestPaySessionResumeFromStore(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
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
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)
	require.Equal(t, PayStateSwapCreated, session.State())

	daemonConn.spentVTXO = &VTXOInfo{
		SpentByTxID: "0123456789abcdef0123456789abcdef" +
			"0123456789abcdef0123456789abcdef",
	}
	daemonConn.indexedPackage = &OORPackageInfo{
		CheckpointPSBTs: [][]byte{
			testCheckpointPSBTWithPreimage(t, preimage[:]),
		},
	}

	resumedClient := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	resumedClient.waitPollInterval = time.Millisecond

	resumed, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateSwapCreated, resumed.State())

	result, err := resumed.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, preimage.Hash(), result.PaymentHash)
	require.Equal(t, preimage, result.Preimage)
	require.Equal(t, "funding-session", result.FundingSessionID)
}

// TestPaySessionCancelDoesNotPersistFailed asserts caller cancellation does
// not durably mark a persisted pay session as Failed while it is waiting for
// funding or claim reconciliation.
func TestPaySessionCancelDoesNotPersistFailed(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
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
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(
		t.Context(), 5*time.Millisecond,
	)
	defer cancel()

	_, err = session.Wait(waitCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	resumedClient := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	resumed, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.NotEqual(t, PayStateFailed, resumed.State())
	require.NotEqual(t, PayStateNeedsIntervention, resumed.State())
}

// TestPaySessionUsesFundingResponseOutpointWhenIndexerRejectsLiveLookup
// asserts the pay FSM can adopt its funded vHTLC from local OOR metadata
// instead of depending on the remote pkScript indexer to reveal the outpoint.
func TestPaySessionUsesFundingResponseOutpointWhenIndexerRejectsLiveLookup(
	t *testing.T) {

	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
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
		blockHeight:   100,
		sendSessionID: "funding-session",
		sendOutpoint:  "funding-session:0",
		liveLookupErr: status.Error(
			codes.Unauthenticated,
			"script not registered for principal",
		),
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	err = session.runUntil(t.Context(), PayStateWaitingForClaim)
	require.NoError(t, err)
	require.Equal(t, PayStateWaitingForClaim, session.State())
	require.Equal(t, "funding-session", session.fundingSessionID)
	require.Equal(t, "funding-session:0", session.vhtlcOutpoint)
	require.EqualValues(t, testInSwapAmountSat, session.vhtlcAmount)
	require.Equal(t, 1, daemonConn.sendPolicyCalls)
	require.Equal(t, 1, daemonConn.liveLookupCalls)
	require.Equal(t, 1, daemonConn.armRecoveryCalls)
	require.Equal(
		t, "funding-session:0",
		daemonConn.lastArmRecovery.GetVtxoOutpoint(),
	)

	reloaded, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateWaitingForClaim, reloaded.State())
	require.Equal(t, "funding-session:0", reloaded.vhtlcOutpoint)
}

// TestPaySessionResumeFundingGraceSkipsImmediateResend asserts a resumed pay
// session in the accepted-but-not-yet-persisted funding window does not
// immediately resend funding while the ambiguity grace period is still active.
func TestPaySessionResumeFundingGraceSkipsImmediateResend(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	now := time.Unix(1_700_000_000, 0)
	grace := 50 * time.Millisecond

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       144,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: now.Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		sendSessionID: "funding-session",
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond
	client.fundingResumeGracePeriod = grace
	client.now = func() time.Time { return now }

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	err = session.mutateAndPersist(t.Context(), func() error {
		return session.transition(payEventFundingInitiated)
	})
	require.NoError(t, err)

	resumedClient := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	resumedClient.waitPollInterval = time.Millisecond
	resumedClient.fundingResumeGracePeriod = grace
	resumedClient.now = func() time.Time { return now }

	resumed, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateFundingInitiated, resumed.State())

	waitCtx, cancel := context.WithTimeout(
		t.Context(), 100*time.Millisecond,
	)
	defer cancel()

	_, err = resumed.Wait(waitCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 0, daemonConn.sendPolicyCalls)
	require.Equal(t, PayStateFundingInitiated, resumed.State())
}

// TestPaySessionResumeFundingGraceEventuallyRetries asserts a resumed pay
// session retries funding after the ambiguity grace period elapses without the
// vHTLC appearing.
func TestPaySessionResumeFundingGraceEventuallyRetries(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	start := time.Unix(1_700_000_000, 0)
	grace := 10 * time.Millisecond

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       144,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: start.Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		sendSessionID: "funding-session",
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond
	client.fundingResumeGracePeriod = grace
	client.now = func() time.Time { return start }

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	err = session.mutateAndPersist(t.Context(), func() error {
		return session.transition(payEventFundingInitiated)
	})
	require.NoError(t, err)

	resumedClient := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	resumedClient.waitPollInterval = time.Millisecond
	resumedClient.fundingResumeGracePeriod = grace
	resumedClient.now = func() time.Time {
		return start.Add(2 * grace)
	}

	resumed, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(
		t.Context(), 5*time.Millisecond,
	)
	defer cancel()

	_, err = resumed.Wait(waitCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, daemonConn.sendPolicyCalls)
	require.Equal(t, PayStateFundingInitiated, resumed.State())

	reloaded, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, "funding-session", reloaded.fundingSessionID)
	require.Equal(t, PayStateFundingInitiated, reloaded.State())
}

// TestPaySessionResumeDoesNotDoubleFireVHTLCFunded is a regression test for a
// pay-FSM resume defect (reported as a settled Lightning payment shown as
// FAILED). A session resumed in FundingInitiated advances to VHTLCFunded via
// local funding metadata, then the SAME waitForFundedVHTLC loop observes the
// vHTLC live and fires OnVHTLCFunded a second time — this time from
// VHTLCFunded, which has no such transition — so the swap is wrongly marked
// Failed even though its vHTLC is funded and the server can still claim it.
//
// The fix belongs in the pay FSM: guard the funded transition on the current
// state (as markVHTLCFundedFromLocalMetadata already does) and stop the loop
// once VHTLCFunded is reached. This test asserts the desired post-fix
// behaviour, so it FAILS on the current code (the resume errors with "invalid
// pay transition VHTLCFunded -> OnVHTLCFunded" and lands in PayStateFailed) and
// PASSES once the FSM stops re-firing OnVHTLCFunded.
func TestPaySessionResumeDoesNotDoubleFireVHTLCFunded(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
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

	// The daemon returns a funding outpoint (so the resume marks the vHTLC
	// funded from local metadata) and reports the vHTLC live only after the
	// funding send — the exact ordering that makes the loop re-fire
	// OnVHTLCFunded from VHTLCFunded.
	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		blockHeight:   50,
		sendSessionID: "funding-session",
		sendOutpoint:  "funding:0",
	}
	daemonConn.liveLookupHook = func(_ int) (*VTXOInfo, error) {
		if daemonConn.sendPolicyCalls == 0 {
			return nil, unregisteredScriptIndexerErr()
		}

		return &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: testInSwapAmountSat,
		}, nil
	}

	// Seed a session persisted in FundingInitiated with no funding session
	// id: the state a client rests in when it records the funding intent
	// but stops before the OOR funding send is submitted.
	client := configureTestPayClient(
		NewSwapClientWithStore(serverConn, daemonConn, nil, nil, store),
	)
	client.waitPollInterval = time.Millisecond
	client.refundLocktimeBuffer = 0
	client.fundingResumeGracePeriod = 0

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)
	require.NoError(
		t,
		session.mutateAndPersist(
			t.Context(),
			func() error {
				return session.transition(
					payEventFundingInitiated,
				)
			},
		),
	)

	resumedClient := configureTestPayClient(
		NewSwapClientWithStore(serverConn, daemonConn, nil, nil, store),
	)
	resumedClient.waitPollInterval = time.Millisecond
	resumedClient.refundLocktimeBuffer = 0
	resumedClient.fundingResumeGracePeriod = 0

	resumed, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateFundingInitiated, resumed.State())

	runCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	// The resumed swap must advance to WaitingForClaim, not be failed by a
	// re-fired OnVHTLCFunded.
	err = resumed.runUntil(runCtx, PayStateWaitingForClaim)
	require.NoError(
		t, err,
		"resume re-fired OnVHTLCFunded and failed a funded swap",
	)
	require.Equal(t, PayStateWaitingForClaim, resumed.State())
}

// TestPaySessionRefundsAmountMismatch asserts the client preserves mismatch
// context while still sweeping the funded vHTLC back once refund matures.
func TestPaySessionRefundsAmountMismatch(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	refundScript, err := txscript.PayToTaprootScript(clientPriv.PubKey())
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
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
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
		blockHeight: 144,
		vhtlc: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: 41_999,
		},
		receiveInfo: &ReceiveInfo{
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
			PkScript:    refundScript,
		},
		sendSessionID: "refund-session",
		spendOnCustom: true,
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	_, err = session.Wait(t.Context())
	require.ErrorIs(t, err, ErrSwapRefunded)

	resumed, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateRefunded, resumed.State())
	require.Contains(t, resumed.TerminalReason(), "does not match quote")
	require.Empty(t, resumed.InterventionReason())
	require.Equal(t, "funding:0", resumed.vhtlcOutpoint)
	require.EqualValues(t, 41_999, resumed.vhtlcAmount)
	require.Equal(t, "refund-session", resumed.refundSessionID)
}

// TestPaySessionFailsNearRefundLocktime asserts the client refuses to submit
// pay-side funding when the refund locktime is already imminent.
func TestPaySessionFailsNearRefundLocktime(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       100,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
		blockHeight: 99,
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	_, err = session.Wait(t.Context())
	require.ErrorContains(t, err, "refund locktime")
	require.Equal(t, 0, daemonConn.sendPolicyCalls)

	resumed, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateFailed, resumed.State())
	require.Contains(t, resumed.TerminalReason(), "refund locktime")
	require.Empty(t, resumed.InterventionReason())
}

// TestPaySessionExpiresBeforeUnsafeLateFunding asserts the client refuses to
// start funding when the persisted funding deadline is already effectively
// exhausted.
func TestPaySessionExpiresBeforeUnsafeLateFunding(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	now := time.Unix(1_700_000_000, 0)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       200,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: now.Add(2 * time.Second),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
		blockHeight: 100,
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.now = func() time.Time { return now }
	client.fundingExpiryBuffer = 5 * time.Second

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	_, err = session.Wait(t.Context())
	require.ErrorIs(t, err, errSwapExpired)
	require.Equal(t, 0, daemonConn.sendPolicyCalls)

	resumed, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateExpired, resumed.State())
}

// TestPaySessionNeedsInterventionOnSpentWithoutPreimage asserts the client
// preserves operator context when the funded vHTLC is authoritatively spent
// but no matching claim preimage can be recovered.
func TestPaySessionNeedsInterventionOnSpentWithoutPreimage(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    testInSwapAmountSat,
			FeeSat:       testInSwapFeeSat,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       200,
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
		blockHeight:   100,
		sendSessionID: "funding-session",
		spentVTXO: &VTXOInfo{
			Outpoint:    "funding:0",
			AmountSat:   testInSwapAmountSat,
			SpentByTxID: "deadbeef",
		},
		indexedPackage: &OORPackageInfo{},
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartPayViaLightning(
		t.Context(), invoice, testInSwapFeeSat,
	)
	require.NoError(t, err)

	_, err = session.Wait(t.Context())
	require.ErrorContains(t, err, "spent without claim preimage")

	resumed, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateNeedsIntervention, resumed.State())
	require.Contains(
		t, resumed.InterventionReason(),
		"spent without claim preimage",
	)
	require.Equal(t, "funding:0", resumed.vhtlcOutpoint)
	require.EqualValues(t, testInSwapAmountSat, resumed.vhtlcAmount)
}

// TestWaitForInSwapClaimObservationToleratesPreimageLag asserts an indexed
// spend does not become NeedsIntervention before the preimage lookup retry
// window has a chance to catch up.
func TestWaitForInSwapClaimObservationToleratesPreimageLag(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		spentVTXO: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: testInSwapAmountSat,
			SpentByTxID: "0123456789abcdef0123456789abcdef" +
				"0123456789abcdef0123456789abcdef",
		},
		indexedPackages: []*OORPackageInfo{
			{},
			{
				CheckpointPSBTs: [][]byte{
					testCheckpointPSBTWithPreimage(
						t, preimage[:],
					),
				},
			},
		},
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	foundPreimage, spentVTXO, err := client.waitForInSwapClaimObservation(
		t.Context(), preimage.Hash(), []byte{0x51},
	)
	require.NoError(t, err)
	require.Nil(t, spentVTXO)
	require.Equal(t, preimage, *foundPreimage)
}

// TestFindInSwapClaimObservationPropagatesListSpentError asserts reconciliation
// does not silently swallow local spent-VTXO query failures.
func TestFindInSwapClaimObservationPropagatesListSpentError(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		listSpentErr: errors.New("spent lookup failed"),
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)

	foundPreimage, spentVTXO, err := client.findInSwapClaimObservation(
		t.Context(), preimage.Hash(), []byte{0x51},
	)
	require.Nil(t, foundPreimage)
	require.Nil(t, spentVTXO)
	require.ErrorContains(t, err, "spent lookup failed")
}

// TestWaitForSpentVTXOPreimageUsesSpendingSession asserts the SDK fetches the
// checkpoints of the OOR session that spent the funded vHTLC when the spent
// vHTLC's own package does not carry the preimage.
func TestWaitForSpentVTXOPreimageUsesSpendingSession(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		spentVTXO: &VTXOInfo{
			SpentByTxID: "0123456789abcdef0123456789abcdef" +
				"0123456789abcdef0123456789abcdef",
		},
		indexedPackage: &OORPackageInfo{
			CheckpointPSBTs: [][]byte{
				testCheckpointPSBTWithPreimage(
					t, preimage[:],
				),
			},
		},
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	client.waitPollInterval = time.Millisecond

	result, err := client.waitForSpentVTXOPreimage(
		t.Context(), preimage.Hash(), []byte{0x51},
		time.Now().Add(time.Second),
	)
	require.NoError(t, err)
	require.Equal(t, preimage, *result)
}

// TestWaitForSpentVTXOPreimageUsesLocalSpentPackages asserts the SDK prefers
// locally persisted spent-VTXO checkpoints when they already carry the claim
// preimage.
func TestWaitForSpentVTXOPreimageUsesLocalSpentPackages(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		spentVTXOs: []VTXOInfo{{
			PkScript: []byte{
				0x51,
			},
			FinalCheckpointPSBTs: [][]byte{
				testCheckpointPSBTWithPreimage(
					t, preimage[:],
				),
			},
		}},
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	client.waitPollInterval = time.Millisecond

	result, err := client.waitForSpentVTXOPreimage(
		t.Context(), preimage.Hash(), []byte{0x51},
		time.Now().Add(time.Second),
	)
	require.NoError(t, err)
	require.Equal(t, preimage, *result)
}

// TestWaitForSpentVTXOPreimageFallsBackToLivePackages asserts the SDK can
// recover the claim preimage from a received live VTXO package when the spent
// vHTLC itself is not exposed as a local spent VTXO.
func TestWaitForSpentVTXOPreimageFallsBackToLivePackages(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		liveVTXOs: []VTXOInfo{{
			FinalCheckpointPSBTs: [][]byte{
				testCheckpointPSBTWithPreimage(
					t, preimage[:],
				),
			},
		}},
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	client.waitPollInterval = time.Millisecond

	result, err := client.waitForSpentVTXOPreimage(
		t.Context(), preimage.Hash(), []byte{0x51},
		time.Now().Add(time.Second),
	)
	require.NoError(t, err)
	require.Equal(t, preimage, *result)
}

// testCheckpointPSBTWithPreimage encodes one finalized checkpoint PSBT that
// carries preimage in a taproot script-spend signature slot.
func testCheckpointPSBTWithPreimage(t *testing.T, preimage []byte) []byte {
	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{})
	tx.AddTxOut(&wire.TxOut{
		Value:    1,
		PkScript: []byte{0x51},
	})

	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	var witness bytes.Buffer
	err = wire.WriteVarInt(&witness, 0, 1)
	require.NoError(t, err)

	err = wire.WriteVarBytes(&witness, 0, preimage)
	require.NoError(t, err)

	packet.Inputs[0].FinalScriptWitness = witness.Bytes()

	var buf bytes.Buffer
	err = packet.Serialize(&buf)
	require.NoError(t, err)

	return buf.Bytes()
}
