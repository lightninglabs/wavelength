package swaps

import (
	"context"
	"errors"
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/stretchr/testify/require"
)

// SendOORWithCustomInputsToAddress records an exact-address claim separately
// from owner-key claims so tests detect output-key reinterpretation.
func (d *testDaemonConn) SendOORWithCustomInputsToAddress(_ context.Context,
	address string, _ int64, inputs []CustomInput) (string, error) {

	d.sendCustomCalls++
	d.lastClaimAddress = address
	d.lastClaimInput = append([]CustomInput(nil), inputs...)

	return d.sendSessionID, d.sendCustomErr
}

// externalReceiveFixture prepares a durable receive with a distinct
// recipient's standard Ark script and test-side services.
func externalReceiveFixture(t *testing.T) (*SwapClient, *testDaemonConn, string,
	[]byte) {

	t.Helper()
	owner, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	server, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipient, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	template, err := arkscript.EncodeStandardVTXOTemplate(
		recipient.PubKey(), operator.PubKey(), 144,
	)
	require.NoError(t, err)
	policy, err := arkscript.DecodePolicyTemplate(template)
	require.NoError(t, err)
	script, err := policy.PkScript()
	require.NoError(t, err)
	address, err := btcaddr.NewAddressTaproot(
		script[2:], &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	serverKey := server.PubKey().SerializeCompressed()
	conn := &testSwapServerConn{
		hint: &RouteHint{
			NodeID:    serverKey,
			ChannelID: 99,
		},
		cfg: &VHTLCConfig{
			RefundLocktime:                       300,
			UnilateralClaimDelay:                 12,
			UnilateralRefundDelay:                24,
			UnilateralRefundWithoutReceiverDelay: 36,
			SwapServerPubkey:                     serverKey,
		},
	}
	daemon := &testDaemonConn{
		identityKey: owner.PubKey(),
		operatorKey: operator.PubKey(),
		vhtlc: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: 42_000,
		},
		sendSessionID: "external-claim",
	}
	creator := &testInvoiceCreator{
		invoice: &invoices.Invoice{
			PaymentRequest: []byte("lnrtest1swap"),
		},
	}
	client := NewSwapClientWithStore(
		conn, daemon, nil, creator, newTestSwapStore(t),
	)
	client.SetChainParams(&chaincfg.RegressionNetParams)
	client.claimMaxAttempts = 1
	client.claimResumeGracePeriod = 0
	client.waitPollInterval = time.Millisecond
	useTestOnionDecoder(client, 42_000)

	return client, daemon, address.EncodeAddress(), script
}

// TestExternalReceiveResumesClaim verifies invoice setup, funding, a failed
// claim, restart, and completion all retain the recipient's exact script.
func TestExternalReceiveResumesClaim(t *testing.T) {
	t.Parallel()
	client, daemon, address, script := externalReceiveFixture(t)
	session, err := client.StartReceiveViaLightningWithOptions(
		t.Context(), 42_000, ReceiveOptions{
			ClaimAddress: address,
		},
	)
	require.NoError(t, err)
	require.Zero(t, daemon.receiveAllocCalls)
	require.Empty(t, session.claimReceivePubKey)
	require.Equal(t, script, session.claimReceiveScript)

	// Resume from the invoice before funding arrives.
	session, err = client.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	_, _, err = session.WaitForFunding(t.Context())
	require.NoError(t, err)
	require.Equal(t, script, daemon.lastArmRecovery.GetDestinationScript())

	// A failed submission must retain the same destination after restart.
	sendErr := errors.New("temporary claim failure")
	daemon.sendCustomErr = sendErr
	require.NoError(
		t,
		session.mutateAndPersist(
			t.Context(),
			func() error {
				return session.transition(
					receiveEventClaimInitiated,
				)
			},
		),
	)
	err = session.claimFundedVHTLC(t.Context())
	// The FSM schedules a retry instead of terminating on submission error.
	require.NoError(t, err)
	require.Equal(t, ReceiveStateClaimInitiated, session.State())
	require.Equal(t, 1, daemon.sendCustomCalls)
	require.Equal(t, address, daemon.lastClaimAddress)
	daemon.sendCustomErr = nil
	session, err = client.ResumeReceiveViaLightning(
		t.Context(), session.PaymentHash,
	)
	require.NoError(t, err)
	result, err := session.Wait(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 42_000, result.AmountSat)
	require.Equal(t, address, daemon.lastClaimAddress)
	require.Empty(t, daemon.lastClaimPubKey)
	require.Zero(t, daemon.receiveAllocCalls)
	require.Len(t, daemon.lastClaimInput, 1)
	require.NotEmpty(t, daemon.lastClaimInput[0].SpendPath)
	summary, err := client.GetSwapSummary(t.Context(), session.PaymentHash)
	require.NoError(t, err)
	require.Equal(t, address, summary.ClaimAddress)
	require.False(t, summary.Pending)
}

// TestExternalReceiveRejectsInvalidAddress verifies invalid destinations fail
// before allocating a route or invoice.
func TestExternalReceiveRejectsInvalidAddress(t *testing.T) {
	t.Parallel()
	client, daemon, _, script := externalReceiveFixture(t)
	wrongNetwork, err := btcaddr.NewAddressTaproot(
		script[2:], &chaincfg.MainNetParams,
	)
	require.NoError(t, err)
	nonTaproot, err := btcaddr.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	for _, address := range []string{
		"invalid",
		wrongNetwork.EncodeAddress(),
		nonTaproot.EncodeAddress(),
	} {
		_, err := client.StartReceiveViaLightningWithOptions(
			t.Context(), 42_000, ReceiveOptions{
				ClaimAddress: address,
			},
		)
		require.ErrorIs(t, err, ErrInvalidClaimAddress)
	}
	require.Zero(t, daemon.receiveAllocCalls)
	server, ok := client.server.(*testSwapServerConn)
	require.True(t, ok)
	require.Nil(t, server.lastVhtlcPubkey)
}

// TestExternalReceiveRejectsCredits prevents server quote metadata from
// silently forwarding local credit or substituting a local credit receive.
func TestExternalReceiveRejectsCredits(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"attached", "credit", "padded"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			client, daemon, address, _ := externalReceiveFixture(t)
			server, ok := client.server.(*testSwapServerConn)
			require.True(t, ok)
			server.routeQuote = &OutSwapQuote{
				RouteHintPaths: [][]*RouteHint{
					{
						server.hint,
					},
				},
			}
			switch mode {
			case "attached":
				server.routeQuote.AttachedCreditSat = 500

			case "credit":
				server.routeQuote.SettlementType =
					SettlementTypeCredit

			case "padded":
				server.routeQuote.VHTLCAmountSat = 42_500
			}
			_, err := client.StartReceiveViaLightningWithOptions(
				t.Context(), 42_000, ReceiveOptions{
					ClaimAddress: address,
				},
			)
			require.ErrorIs(t, err, ErrExternalReceiveCredits)
			require.Zero(t, daemon.receiveAllocCalls)
			creator, ok := client.invoiceGen.(*testInvoiceCreator)
			require.True(t, ok)
			require.Nil(t, creator.lastAuthKey)
		})
	}
}
