package swaps

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// CreateCredit records one test credit funding request.
func (c *testInSwapServerConn) CreateCredit(_ context.Context,
	accountPubKey []byte, req CreateCreditRequest) (*CreditOperation,
	error) {

	c.creditCreateCalls++
	c.creditAccountKey = append([]byte(nil), accountPubKey...)
	c.creditCreateReq = req

	return c.creditCreateResp, nil
}

// RedeemCredit is unused in payment-intent tests.
func (c *testInSwapServerConn) RedeemCredit(context.Context, []byte,
	RedeemCreditRequest) (*CreditRedemption, error) {

	return nil, fmt.Errorf("unexpected redeem credit request")
}

// ListCredits records one test credit snapshot request.
func (c *testInSwapServerConn) ListCredits(_ context.Context,
	_ []byte, _ uint32) (*CreditSnapshot, error) {

	c.listCreditsCalls++
	if c.listCreditsHook != nil {
		return c.listCreditsHook(c.listCreditsCalls), nil
	}

	return c.listCreditsResp, nil
}

// TestPaymentIntentTopUpCreatesOORBeforePay verifies the durable payment
// runner creates one credit top-up, sends the server-owned OOR, waits until
// ListCredits reports the credits, then starts the normal credit-backed pay.
func TestPaymentIntentTopUpCreatesOORBeforePay(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)
	creditSat := uint64(testInSwapInvoiceSat)
	topUpSat := uint64(1_000)

	serverConn := &testInSwapServerConn{
		quote: &InSwapQuote{
			PaymentHash:      preimage.Hash(),
			InvoiceAmountSat: testInSwapInvoiceSat,
			AmountSat:        0,
			FeeSat:           0,
			Expiry:           time.Now().Add(time.Minute),
			SettlementType:   SettlementTypeCredit,
			CreditQuote: &CreditQuote{
				MustUseCredit:      true,
				CreditShortfallSat: creditSat,
				CreditTopupSat:     topUpSat,
			},
		},
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
		creditCreateResp: &CreditOperation{
			OperationID:    "cr_topup",
			State:          CreditStateAwaitingPayment,
			AmountSat:      topUpSat,
			DestinationKey: []byte{9, 9, 9},
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		sendSessionID: "oor-topup",
	}
	serverConn.listCreditsHook = func(_ int) *CreditSnapshot {
		opState := CreditStateAwaitingPayment
		availableSat := uint64(0)
		if daemonConn.sendPubKeyCalls > 0 {
			opState = CreditStateCredited
			availableSat = creditSat
		}

		return &CreditSnapshot{
			AvailableSat: availableSat,
			Operations: []CreditOperation{{
				OperationID: "cr_topup",
				State:       opState,
				AmountSat:   topUpSat,
			}},
		}
	}

	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)

	session, err := client.StartPayViaLightningWithCreditTopUp(
		ctx, invoice, testInSwapFeeSat, creditSat, topUpSat,
	)
	require.NoError(t, err)
	require.Equal(t, PayStateCompleted, session.State())

	paymentHash := preimage.Hash()
	require.Equal(t, 1, serverConn.creditCreateCalls)
	require.Equal(
		t, clientPriv.PubKey().SerializeCompressed(),
		serverConn.creditAccountKey,
	)
	require.Equal(t, topUpSat, serverConn.creditCreateReq.AmountSat)
	require.Equal(
		t, creditTopUpID(paymentHash),
		serverConn.creditCreateReq.IdempotencyKey,
	)
	require.Equal(t, 1, daemonConn.sendPubKeyCalls)
	require.Equal(t, []byte{9, 9, 9}, daemonConn.lastSendPubKey)
	require.Equal(
		t, creditTopUpID(paymentHash), daemonConn.lastSendTopUpKey,
	)
	require.Equal(t, 1, serverConn.createCalls)
	require.Equal(t, creditSat, serverConn.createMaxCreditSat)

	intents, err := client.ListPendingPaymentIntents(ctx)
	require.NoError(t, err)
	require.Empty(t, intents)
}

// TestPaymentIntentResumeSkipsOORWhenCreditAlreadyAvailable verifies a restart
// after creating the top-up operation reconciles ListCredits before submitting
// another OOR. If the server already credited the operation, the runner starts
// the pay FSM without gifting a second top-up.
func TestPaymentIntentResumeSkipsOORWhenCreditAlreadyAvailable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)
	paymentHash := preimage.Hash()
	creditSat := uint64(testInSwapInvoiceSat)
	topUpSat := uint64(1_000)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:    paymentHash,
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
		listCreditsResp: &CreditSnapshot{
			AvailableSat: creditSat,
			Operations: []CreditOperation{{
				OperationID: "cr_topup",
				State:       CreditStateCredited,
				AmountSat:   topUpSat,
			}},
		},
	}
	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
	}
	client := configureTestPayClient(
		NewSwapClientWithStore(
			serverConn, daemonConn, nil, nil, store,
		),
	)

	intent := &paymentIntent{
		client:               client,
		paymentHash:          paymentHash,
		invoice:              invoice,
		maxFeeSat:            testInSwapFeeSat,
		maxCreditSat:         creditSat,
		maxCreditTopUpSat:    topUpSat,
		state:                PaymentIntentCreditTopUpCreated,
		creditIdempotencyKey: creditTopUpID(paymentHash),
		creditOperationID:    "cr_topup",
		creditTopUpSat:       topUpSat,
		creditDestinationKey: []byte{9, 9, 9},
		creditOORSessionID:   "",
		createdAt:            time.Now(),
		updatedAt:            time.Now(),
	}
	require.NoError(t, intent.persist(ctx))

	session, err := client.ResumePaymentIntent(ctx, paymentHash)
	require.NoError(t, err)
	require.Equal(t, PayStateCompleted, session.State())
	require.Zero(t, daemonConn.sendPubKeyCalls)
	require.Zero(t, serverConn.creditCreateCalls)
	require.Equal(t, 1, serverConn.listCreditsCalls)
	require.Equal(t, 1, serverConn.createCalls)
	require.Equal(t, creditSat, serverConn.createMaxCreditSat)
}
