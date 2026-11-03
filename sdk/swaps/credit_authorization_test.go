package swaps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	swapsqlc "github.com/lightninglabs/wavelength/sdk/swaps/sqlc"
	"github.com/stretchr/testify/require"
)

// TestPayCreditAuthorization rejects hostile credit completion before local
// success or Ark funding, including when the caller authorizes zero credit.
func TestPayCreditAuthorization(t *testing.T) {
	for _, capSat := range []uint64{0, 100} {
		t.Run(fmtCreditCap(capSat), func(t *testing.T) {
			key, err := btcec.NewPrivateKey()
			require.NoError(t, err)
			preimage, err := NewPreimage()
			require.NoError(t, err)
			server := &testInSwapServerConn{cfg: &InSwapConfig{
				PaymentHash:    preimage.Hash(),
				SettlementType: SettlementTypeCredit,
				CreditQuote: &CreditQuote{
					CreditAppliedSat: testInSwapInvoiceSat,
				},
				Preimage: &preimage, Expiry: time.
						Now().
						Add(time.Minute),
			}}
			daemon := &testDaemonConn{identityKey: key.PubKey()}
			client := configureTestPayClient(
				NewSwapClient(server, daemon, nil, nil),
			)
			session, err := client.StartPayViaLightningWithCredits(
				t.Context(), testValidPayInvoice(t, preimage),
				testInSwapFeeSat, capSat,
			)
			require.Error(t, err)
			require.Nil(t, session)
			require.Zero(t, daemon.sendPolicyCalls)
		})
	}
}

// fmtCreditCap names the zero and insufficient authorization cases.
func fmtCreditCap(capSat uint64) string {
	if capSat == 0 {
		return "zero"
	}

	return "below applied"
}

// TestPayCreditPlans pins optional, mandatory, mixed and credit-only plans at
// both trust boundaries. Shortfall counts against the creditCapSat before any
// top-up.
func TestPayCreditPlans(t *testing.T) {
	tests := []struct {
		name         string
		rail         SettlementType
		creditCapSat uint64
		plan         *CreditQuote
		completed    bool
		wantErr      bool
	}{
		{
			"no credit",
			SettlementTypeLightning,
			0,
			nil,
			false,
			false,
		},
		{
			"optional zero",
			SettlementTypeMixed,
			0,
			&CreditQuote{
				CreditAppliedSat: 100,
				ArkFundingSat:    900,
			},
			false,
			true,
		},
		{
			"mixed exact",
			SettlementTypeMixed,
			100,
			&CreditQuote{
				CreditAppliedSat: 100,
				ArkFundingSat:    900,
			},
			true,
			false,
		},
		{
			"mixed over creditCapSat",
			SettlementTypeMixed,
			99,
			&CreditQuote{
				CreditAppliedSat: 100,
				ArkFundingSat:    900,
			},
			true,
			true,
		},
		{
			"mixed tiny funding",
			SettlementTypeMixed,
			100,
			&CreditQuote{
				CreditAppliedSat: 100,
				ArkFundingSat:    1,
			},
			true,
			true,
		},
		{
			"credit exact",
			SettlementTypeCredit,
			1000,
			&CreditQuote{
				CreditAppliedSat: 1000,
			},
			true,
			false,
		},
		{
			"credit over creditCapSat",
			SettlementTypeCredit,
			999,
			&CreditQuote{
				CreditAppliedSat: 1000,
			},
			true,
			true,
		},
		{
			"credit missing plan",
			SettlementTypeCredit,
			1000,
			nil,
			true,
			true,
		},
		{
			"credit missing amount",
			SettlementTypeCredit,
			1000,
			&CreditQuote{},
			true,
			true,
		},
		{
			"mandatory zero",
			SettlementTypeCredit,
			0,
			&CreditQuote{
				MustUseCredit:      true,
				CreditShortfallSat: 1000,
				CreditTopupSat:     1000,
			},
			false,
			true,
		},
		{
			"mandatory insufficient creditCapSat",
			SettlementTypeCredit,
			999,
			&CreditQuote{
				MustUseCredit:      true,
				CreditAppliedSat:   100,
				CreditShortfallSat: 900,
				CreditTopupSat:     900,
			},
			false,
			true,
		},
		{
			"mandatory authorized topup",
			SettlementTypeCredit,
			1000,
			&CreditQuote{
				MustUseCredit:      true,
				CreditAppliedSat:   100,
				CreditShortfallSat: 900,
				CreditTopupSat:     900,
			},
			false,
			false,
		},
		{
			"completion still needs topup",
			SettlementTypeCredit,
			1000,
			&CreditQuote{
				CreditAppliedSat:   100,
				CreditShortfallSat: 900,
				CreditTopupSat:     900,
			},
			true,
			true,
		},
		{
			"hidden credit leg",
			SettlementTypeLightning,
			1000,
			&CreditQuote{
				CreditAppliedSat: 1000,
			},
			true,
			true,
		},
		{
			"overflow",
			SettlementTypeCredit,
			^uint64(0),
			&CreditQuote{
				CreditAppliedSat:   ^uint64(0),
				CreditShortfallSat: 1,
			},
			false,
			true,
		},
		{
			"released routing budget",
			SettlementTypeCredit,
			1010,
			&CreditQuote{
				CreditAppliedSat: 1010,
			},
			true,
			false,
		},
		{
			"unknown rail",
			SettlementType("unknown"),
			0,
			nil,
			false,
			true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validatePayCredit(
				test.rail, test.plan, test.creditCapSat, 1000,
				test.completed,
			)
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestPayCreditRestartReplay preserves creditCapSat, identity and accepted plan
// after closing SQLite. Exact replay returns the durable proof without another
// create.
func TestPayCreditRestartReplay(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)
	server := &testInSwapServerConn{cfg: &InSwapConfig{
		PaymentHash:    preimage.Hash(),
		SettlementType: SettlementTypeCredit,
		CreditQuote: &CreditQuote{
			CreditAppliedSat: testInSwapInvoiceSat,
		},
		Preimage: &preimage, Expiry: time.Now().Add(time.Minute),
	}}
	daemon := &testDaemonConn{identityKey: key.PubKey()}
	cfg := &SqliteStoreConfig{
		DatabaseFileName: filepath.Join(t.TempDir(), "swaps.db"),
	}
	store, err := NewSqliteStore(cfg, nil)
	require.NoError(t, err)
	client := configureTestPayClient(
		NewSwapClientWithStore(server, daemon, nil, nil, store),
	)
	session, err := client.StartPayViaLightningWithCredits(
		t.Context(), invoice, testInSwapFeeSat, ^uint64(0),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateCompleted, session.State())
	require.NoError(t, store.Close())
	store, err = NewSqliteStore(cfg, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	client = configureTestPayClient(
		NewSwapClientWithStore(server, daemon, nil, nil, store),
	)
	restored, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, ^uint64(0), restored.maxCreditSat)
	require.Equal(t, server.cfg.CreditQuote, restored.cfg.CreditQuote)
	result, err := restored.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, preimage, result.Preimage)
	summary, err := client.GetSwapSummary(t.Context(), preimage.Hash())
	require.NoError(t, err)
	require.Equal(t, server.cfg.CreditQuote, summary.CreditQuote)
	_, err = client.StartPayViaLightningWithCredits(
		t.Context(), invoice, testInSwapFeeSat, ^uint64(0),
	)
	require.NoError(t, err)
	_, err = client.StartPayViaLightningWithCredits(
		t.Context(), invoice, testInSwapFeeSat, testInSwapInvoiceSat,
	)
	require.ErrorContains(t, err, "differs from original")
	require.NoError(
		t,
		client.ValidatePayAuthorization(
			t.Context(), invoice, testInSwapFeeSat, ^uint64(0),
		),
	)
	require.Error(
		t,
		client.ValidatePayAuthorization(
			t.Context(), invoice, testInSwapFeeSat, 0,
		),
	)
	other, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	daemon.identityKey = other.PubKey()
	_, err = client.StartPayViaLightningWithCredits(
		t.Context(), invoice, testInSwapFeeSat, ^uint64(0),
	)
	require.ErrorContains(t, err, "differs from original")
	require.Equal(t, 1, server.createCalls)
}

// TestPayAuthorizationConcurrentAdmission gives two sufficient caps the same
// operation identity. SQLite admits exactly one immutable request fingerprint.
func TestPayAuthorizationConcurrentAdmission(t *testing.T) {
	store := newTestSwapStore(t)
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)
	client := configureTestPayClient(
		NewSwapClientWithStore(nil, nil, nil, nil, store),
	)
	caps := []uint64{50000, 60000}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, creditCapSat := range caps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.authorizePay(
				t.Context(), invoice, 123, creditCapSat,
				key.PubKey(),
			)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	var accepted, refused int
	for err := range results {
		if err == nil {
			accepted++
		} else {
			require.ErrorContains(t, err, "differs from original")
			refused++
		}
	}
	require.Equal(t, 1, accepted)
	require.Equal(t, 1, refused)
	hash := preimage.Hash()
	auth, err := store.queries.GetPayAuthorization(t.Context(), hash[:])
	require.NoError(t, err)
	creditCapSat, err := strconv.ParseUint(auth.MaxCreditSat, 10, 64)
	require.NoError(t, err)
	_, err = client.authorizePay(
		t.Context(), invoice, 123, creditCapSat, key.PubKey(),
	)
	require.NoError(t, err)
}

// TestPayAuthorizationSurvivesLostPersistence injects the crash window after
// credit completion but before its local session row commits. Restart retries
// with the original identity and creditCapSat; changed authority never reaches
// create.
func TestPayAuthorizationSurvivesLostPersistence(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)
	server := &testInSwapServerConn{cfg: &InSwapConfig{
		PaymentHash:    preimage.Hash(),
		SettlementType: SettlementTypeCredit,
		CreditQuote: &CreditQuote{
			CreditAppliedSat: testInSwapInvoiceSat,
		},
		Preimage: &preimage, Expiry: time.Now().Add(time.Minute),
	}}
	daemon := &testDaemonConn{identityKey: key.PubKey()}
	cfg := &SqliteStoreConfig{
		DatabaseFileName: filepath.Join(t.TempDir(), "swaps.db"),
	}
	store, err := NewSqliteStore(cfg, nil)
	require.NoError(t, err)
	lost := errors.New("injected post-create persistence failure")
	store.queries = swapsqlc.New(
		&failAtExecDBTX{
			db:     store.DB(),
			err:    lost,
			failAt: 3,
		},
	)
	client := configureTestPayClient(
		NewSwapClientWithStore(server, daemon, nil, nil, store),
	)
	_, err = client.StartPayViaLightningWithCredits(
		t.Context(), invoice, 123, 50000,
	)
	require.ErrorIs(t, err, lost)
	require.Equal(t, 1, server.createCalls)
	require.Equal(t, uint64(50000), server.createMaxCreditSat)
	require.NoError(t, store.Close())
	store, err = NewSqliteStore(cfg, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	client = configureTestPayClient(
		NewSwapClientWithStore(server, daemon, nil, nil, store),
	)
	_, err = client.StartPayViaLightningWithCredits(
		t.Context(), invoice, 123, 60000,
	)
	require.ErrorContains(t, err, "differs from original")
	require.Equal(t, 1, server.createCalls)
	session, err := client.StartPayViaLightningWithCredits(
		t.Context(), invoice, 123, 50000,
	)
	require.NoError(t, err)
	require.Equal(t, PayStateCompleted, session.State())
	require.Equal(t, 2, server.createCalls)
	require.Equal(t, uint64(50000), server.createMaxCreditSat)
}

// creditPreviewServer records the exact account and cap used for
// preview.
type creditPreviewServer struct {
	*testInSwapServerConn
	account      []byte
	creditCapSat uint64
}

// QuoteInSwapWithCredits captures caller authority without creating a swap.
func (s *creditPreviewServer) QuoteInSwapWithCredits(ctx context.Context,
	invoice string, fee uint64, account []byte, creditCapSat uint64) (
	*InSwapQuote, error) {

	s.account = append([]byte(nil), account...)
	s.creditCapSat = creditCapSat

	return s.QuoteInSwap(ctx, invoice, fee)
}

// TestPayCreditPreviewAndMixedCreate checks both SDK entry points against a
// server that substitutes its own reservation. Authorized mixed plans retain
// the caller cap and exact credit leg after durable resume.
func TestPayCreditPreviewAndMixedCreate(t *testing.T) {
	for _, rail := range []SettlementType{
		SettlementTypeCredit,
		SettlementTypeMixed,
	} {
		for _, capSat := range []uint64{0, 100, testInSwapInvoiceSat} {
			t.Run(fmt.Sprintf("%s cap %d",
				rail, capSat), func(t *testing.T) {
				testCreditPreviewAndCreate(t, rail, capSat)
			})
		}
	}
}

// testCreditPreviewAndCreate exercises hostile replies and exact authority
// retention through an authorized mixed or credit-only create and restore.
func testCreditPreviewAndCreate(t *testing.T, rail SettlementType,
	creditCapSat uint64) {

	t.Helper()

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	preimage, err := NewPreimage()
	require.NoError(t, err)
	invoice := testValidPayInvoice(t, preimage)
	applied := uint64(testInSwapInvoiceSat)
	funding := uint64(0)
	fee := uint64(0)
	if rail == SettlementTypeMixed {
		applied = 1000
		fee = testInSwapFeeSat
		funding = testInSwapAmountSat - applied
	}
	plan := &CreditQuote{
		CreditAppliedSat: applied,
		ArkFundingSat:    funding,
	}
	cfg := testInSwapConfig(
		key.PubKey(), preimage, time.Now().Add(time.Minute),
	)
	cfg.SettlementType, cfg.CreditQuote = rail, plan
	cfg.AmountSat, cfg.FeeSat = int64(funding), fee
	cfg.Preimage = &preimage
	server := &creditPreviewServer{
		testInSwapServerConn: &testInSwapServerConn{
			cfg: cfg,
			quote: &InSwapQuote{
				PaymentHash:      preimage.Hash(),
				InvoiceAmountSat: testInSwapInvoiceSat,
				AmountSat:        funding,
				FeeSat:           fee,
				SettlementType:   rail,
				CreditQuote:      plan,
			},
		},
	}
	daemon := &testDaemonConn{
		identityKey: key.PubKey(),
		operatorKey: key.PubKey(),
	}
	store := newTestSwapStore(t)
	client := configureTestPayClient(
		NewSwapClientWithStore(
			server, daemon, nil, nil, store,
		),
	)
	_, err = client.QuotePayViaLightningWithCredits(
		t.Context(), invoice, 123, creditCapSat,
	)
	require.Equal(
		t, creditCapSat, server.creditCapSat,
	)
	require.Equal(
		t, key.PubKey().SerializeCompressed(),
		server.account,
	)
	require.Zero(t, server.createCalls)
	if creditCapSat < applied {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
	}
	startPay := client.StartPayViaLightningWithCredits
	session, err := startPay(
		t.Context(), invoice, 123, creditCapSat,
	)
	require.Equal(
		t, creditCapSat,
		server.createMaxCreditSat,
	)
	if creditCapSat < applied {
		require.Error(t, err)
		require.Nil(t, session)
	} else {
		require.NoError(t, err)
		resumePay := client.ResumePayViaLightning
		restored, err := resumePay(
			t.Context(), preimage.Hash(),
		)
		require.NoError(t, err)
		require.Equal(
			t, creditCapSat,
			restored.maxCreditSat,
		)
		require.Equal(
			t, plan,
			restored.cfg.CreditQuote,
		)
		if rail == SettlementTypeMixed {
			require.Equal(
				t, PayStateSwapCreated, restored.State(),
			)
		}
	}
	require.Zero(t, daemon.sendPolicyCalls)
}

// TestLegacyPayReplayDoesNotInventAuthority reattaches to a pre-migration row
// without create or a new cap, for both pending recovery and completed proof.
func TestLegacyPayReplayDoesNotInventAuthority(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprint(completed), func(t *testing.T) {
			key, err := btcec.NewPrivateKey()
			require.NoError(t, err)
			preimage, err := NewPreimage()
			require.NoError(t, err)
			invoice := testValidPayInvoice(t, preimage)
			store := newTestSwapStore(t)
			daemon := &testDaemonConn{identityKey: key.PubKey()}
			server := &testInSwapServerConn{}
			client := configureTestPayClient(
				NewSwapClientWithStore(
					server, daemon, nil, nil, store,
				),
			)
			cfg := testInSwapConfig(
				key.PubKey(), preimage,
				time.Now().Add(time.Minute),
			)
			legacy := &paySession{
				client:         client,
				invoice:        invoice,
				maxFeeSat:      123,
				cfg:            cfg,
				state:          PayStateSwapCreated,
				clientPubKey:   key.PubKey(),
				serverPubKey:   key.PubKey(),
				operatorPubKey: key.PubKey(),
			}
			if completed {
				legacy.state = PayStateCompleted
				legacy.preimage = &preimage
			}
			require.NoError(t, legacy.persist(t.Context()))
			replay, err := client.StartPayViaLightningWithCredits(
				t.Context(), invoice, 999, 50000,
			)
			require.NoError(t, err)
			require.Equal(t, legacy.State(), replay.State())
			require.Zero(t, replay.maxCreditSat)
			require.NoError(
				t,
				client.ValidatePayAuthorization(
					t.Context(), invoice, 999, 50000,
				),
			)
			hash := preimage.Hash()
			_, err = store.queries.GetPayAuthorization(
				t.Context(), hash[:],
			)
			require.ErrorIs(t, err, sql.ErrNoRows)
			require.Zero(t, server.createCalls)
		})
	}
}
