//go:build itest

package itest

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/harness"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

const (
	// testBoardingExitDelay is the short CSV delay used by the manual
	// boarding-sweep integration test. Mirrors testVTXOExitDelay in
	// unroll_test.go: 16 blocks is the smallest value that satisfies
	// downstream validators while keeping the mine-past-expiry block
	// budget bounded for regtest.
	testBoardingExitDelay = 16

	// eventBoardingSweepFeePaid is the ledger event type emitted on
	// boarding-sweep confirmation. Mirrors the EventBoardingSweepFeePaid
	// constant in client/ledger/actor.go; duplicated here as a literal so
	// the itest package does not have to import the client ledger
	// package just to read one event-type string.
	eventBoardingSweepFeePaid = "boarding_sweep_fee_paid"
)

// waitForBoardingSweepConfirmed polls ListBoardingSweeps until exactly one
// sweep with the expected txid reaches the "confirmed" status. The matched
// BoardingSweep proto is returned so callers can assert per-input status
// and accounting fields.
func waitForBoardingSweepConfirmed(t *testing.T,
	client daemonrpc.DaemonServiceClient,
	expectedTxID string) *daemonrpc.BoardingSweep {

	t.Helper()

	var matched *daemonrpc.BoardingSweep
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.ListBoardingSweeps(
			ctx, &daemonrpc.ListBoardingSweepsRequest{},
		)
		if err != nil {
			return false
		}

		for _, sweep := range resp.Sweeps {
			if sweep.Txid != expectedTxID {
				continue
			}

			if sweep.Status != "confirmed" {
				continue
			}

			matched = sweep

			return true
		}

		return false
	}, defaultTimeout, pollInterval,
		"boarding sweep %s never reached confirmed status",
		expectedTxID)

	return matched
}

// waitForBoardingSweptBalance polls GetBalance until BoardingSweptSat
// reaches the expected total and BoardingPendingSweepSat returns to zero.
// The final balance response is returned so callers can spot-check the
// remaining boarding-balance breakdown.
func waitForBoardingSweptBalance(t *testing.T,
	client daemonrpc.DaemonServiceClient,
	expectedSweptSat int64) *daemonrpc.GetBalanceResponse {

	t.Helper()

	var lastResp *daemonrpc.GetBalanceResponse
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.GetBalance(
			ctx, &daemonrpc.GetBalanceRequest{},
		)
		if err != nil {
			return false
		}

		lastResp = resp

		return resp.BoardingSweptSat == expectedSweptSat &&
			resp.BoardingPendingSweepSat == 0
	}, defaultTimeout, pollInterval,
		"boarding swept balance never reached %d sats",
		expectedSweptSat)

	return lastResp
}

// waitForBoardingSweepFeeEntry polls GetFeeHistory until exactly one
// entry with event_type "boarding_sweep_fee_paid" is visible. The matched
// entry is returned so callers can assert accounts and amount.
func waitForBoardingSweepFeeEntry(t *testing.T,
	client daemonrpc.DaemonServiceClient) *daemonrpc.FeeHistoryEntry {

	t.Helper()

	var matched *daemonrpc.FeeHistoryEntry
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.GetFeeHistory(
			ctx, &daemonrpc.GetFeeHistoryRequest{Limit: 100},
		)
		if err != nil {
			return false
		}

		var found *daemonrpc.FeeHistoryEntry
		hits := 0
		for _, entry := range resp.Entries {
			if entry.EventType != eventBoardingSweepFeePaid {
				continue
			}

			found = entry
			hits++
		}

		if hits != 1 {
			return false
		}

		matched = found

		return true
	}, defaultTimeout, pollInterval,
		"GetFeeHistory never reported exactly one %s entry",
		eventBoardingSweepFeePaid)

	return matched
}

// waitForOperatorWalletUTXOAt polls the operator's LND WalletKit until a
// UTXO matching the expected (txid, pkScript) appears in the confirmed
// spendable set. Mirrors the helper in sweep_test.go but matches on a
// known sweep txid (vs. discovering it via mempool scan).
func waitForOperatorWalletUTXOAt(t *testing.T, h *harness.ArkHarness,
	expectedTxID string, expectedPkScript []byte) *lnwallet.Utxo {

	t.Helper()

	var matched *lnwallet.Utxo
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		utxos, err := h.LND.WalletKit.ListUnspent(
			ctx, 1, 1_000_000,
		)
		if err != nil {
			return false
		}

		for _, utxo := range utxos {
			if utxo.OutPoint.Hash.String() != expectedTxID {
				continue
			}

			if !bytes.Equal(utxo.PkScript, expectedPkScript) {
				continue
			}

			matched = utxo

			return true
		}

		return false
	}, defaultTimeout, pollInterval,
		"operator LND never observed swept UTXO at %s",
		expectedTxID)

	return matched
}

// TestBoardingSweepIntegrationExpiredToExternalAddr drives the
// end-to-end manual expired-boarding-sweep flow against a real client
// daemon. The test funds a single boarding UTXO, deliberately skips the
// Board RPC so the intent stays in BoardingStatusConfirmed (sweepable),
// mines past the CSV exit delay, previews and then broadcasts an
// aggregate sweep to an external operator-LND taproot address, and
// asserts the full client-visible surface: ListBoardingSweeps state,
// GetBalance breakdown fields, the operator LND receiving the swept
// output, and the new boarding_sweep_fee_paid ledger entry emitted on
// confirmation.
func TestBoardingSweepIntegrationExpiredToExternalAddr(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	const (
		boardingAmount = btcutil.Amount(2_000_000)

		// testSweepFeeRateSatPerVByte pins the sweep fee rate so the
		// test does not depend on LND's regtest fee estimator, which
		// can return values high enough to trip the wallet actor's
		// 25%-of-input sweep-fee cap (DefaultBoardingSweepMaxFeePercent
		// in client/wallet/boarding_sweep.go).
		testSweepFeeRateSatPerVByte = int64(10)
	)

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
		OperatorConfigMutator: func(cfg *darepo.Config) {
			cfg.Rounds.BoardingExitDelay = testBoardingExitDelay
		},
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	alice := h.StartClientDaemon("alice")

	// The boarding sweep tx is a v3/TRUC parent with a 0-value P2A
	// anchor output. Direct broadcast of the parent alone is rejected
	// by btcwallet's IsDust check on the anchor, so confirmation flows
	// through txconfirm's CPFP path. CPFP needs a confirmed wallet
	// UTXO on the client's daemon-managed wallet for the fee input.
	//
	// We deliberately fund a UTXO SMALLER than boardingAmount: the
	// broadcaster's selectFeeInput picks the smallest confirmed UTXO
	// that covers the CPFP fee (see client/txconfirm/broadcaster.go).
	// Boarding outpoints are watched in LND via ImportTaprootScript
	// and show up in ListUnspent alongside genuine wallet UTXOs; a
	// fee-input larger than the boarding UTXO would lose the smallest-
	// first tiebreak to the boarding outpoint, which LND cannot sign
	// alone (it's a 2-of-2+CSV taproot) and the CPFP child would fail
	// PSBT finalization. 100k sats is well above any CPFP fee at our
	// 10 sat/vbyte test rate and well below the 2M-sat boarding UTXO.
	const cpfpFeeInputAmount = btcutil.Amount(100_000)
	h.FundClientWallet(alice, cpfpFeeInputAmount)

	operatorInfo := getOperatorInfo(t, h)
	require.Equal(t, uint32(testBoardingExitDelay),
		operatorInfo.BoardingExitDelay,
		"operator must advertise the reduced boarding exit delay")

	// Mint a boarding address and fund it; we deliberately do NOT call
	// Board afterwards so the intent stays in BoardingStatusConfirmed
	// and is therefore sweepable past CSV maturity.
	newAddrResp, err := alice.RPCClient.NewAddress(
		t.Context(), &daemonrpc.NewAddressRequest{},
	)
	require.NoError(t, err, "NewAddress RPC failed")
	require.NotEmpty(t, newAddrResp.Address,
		"boarding address should be set")

	fundingTxID := h.Faucet(newAddrResp.Address, boardingAmount)
	t.Logf("Funded boarding address via txid=%s", fundingTxID)

	// Mine one extra block beyond the advertised minimum so the daemon
	// observes the boarding UTXO before we mine past expiry.
	h.Generate(int(operatorInfo.MinConfirmations) + 1)

	confirmedBalance := waitForConfirmedBoardingBalance(
		t, alice.RPCClient, int64(boardingAmount),
	)
	require.Equal(t, int64(boardingAmount),
		confirmedBalance.BoardingConfirmedSat)
	require.Zero(t, confirmedBalance.BoardingPendingSweepSat,
		"no sweep should be in flight yet")
	require.Zero(t, confirmedBalance.BoardingSweptSat,
		"no sweep should have completed yet")

	// Mine past the CSV exit delay so the boarding intent is mature
	// and eligible to be swept via the timeout path.
	h.Generate(testBoardingExitDelay)

	// Preview the aggregate sweep first. The daemon should report a
	// single sweepable output for our funded boarding UTXO and a
	// non-zero estimated fee. The preview txid is the signature of the
	// in-memory build for the current request — we capture it for the
	// trace logs but do NOT use it for downstream assertions (a later
	// broadcast can differ if inputs, fees, or destination change).
	previewCtx, previewCancel := context.WithTimeout(
		t.Context(), defaultTimeout,
	)
	defer previewCancel()
	previewResp, err := alice.RPCClient.SweepBoardingUTXOs(
		previewCtx, &daemonrpc.SweepBoardingUTXOsRequest{
			FeeRateSatPerVbyte: testSweepFeeRateSatPerVByte,
		},
	)
	require.NoError(t, err, "preview SweepBoardingUTXOs RPC failed")
	require.Equal(t, "preview", previewResp.Status,
		"preview response must report status=preview "+
			"(failure_reason=%q)", previewResp.FailureReason)
	require.Len(t, previewResp.SweepableOutputs, 1,
		"only one boarding UTXO was funded")
	require.Equal(t, int64(boardingAmount), previewResp.TotalAmountSat,
		"total_amount_sat must match the funded boarding amount")
	require.Greater(t, previewResp.EstimatedFeeSat, int64(0),
		"estimated_fee_sat must be positive")
	require.Equal(
		t, previewResp.TotalAmountSat-previewResp.EstimatedFeeSat,
		previewResp.NetAmountSat,
		"net_amount_sat must equal total - estimated_fee",
	)
	require.NotEmpty(t, previewResp.Txid, "preview must include a txid")
	boardingOutpoint := previewResp.SweepableOutputs[0].Outpoint
	require.True(t,
		strings.HasPrefix(boardingOutpoint, fundingTxID+":"),
		"sweepable outpoint %q must reference funding tx %s",
		boardingOutpoint, fundingTxID)
	t.Logf("Preview reports %d sweepable sats at %s (fee=%d, net=%d)",
		previewResp.TotalAmountSat, boardingOutpoint,
		previewResp.EstimatedFeeSat, previewResp.NetAmountSat)

	// Mint an external sweep destination from the operator's LND
	// wallet (the same wallet sweep_test.go uses for batch-sweep
	// assertions). This exercises the daemon's external-destination
	// branch: no UTXOCreatedMsg is emitted post-confirmation, and the
	// outflow is captured by the per-input spent rows alone.
	addrCtx, addrCancel := context.WithTimeout(
		t.Context(), defaultSmallTimeout,
	)
	defer addrCancel()
	externalAddr, err := h.LND.WalletKit.NextAddr(
		addrCtx, "", walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	require.NoError(t, err, "operator LND NextAddr failed")
	externalPkScript, err := txscript.PayToAddrScript(externalAddr)
	require.NoError(t, err, "derive pkScript for external sweep address")
	t.Logf("External sweep destination: %s", externalAddr.String())

	// Broadcast the aggregate sweep to the external address.
	broadcastCtx, broadcastCancel := context.WithTimeout(
		t.Context(), defaultTimeout,
	)
	defer broadcastCancel()
	broadcastResp, err := alice.RPCClient.SweepBoardingUTXOs(
		broadcastCtx, &daemonrpc.SweepBoardingUTXOsRequest{
			Broadcast:          true,
			SweepAddress:       externalAddr.String(),
			FeeRateSatPerVbyte: testSweepFeeRateSatPerVByte,
		},
	)
	require.NoError(t, err, "broadcast SweepBoardingUTXOs RPC failed")
	require.Equal(t, "published", broadcastResp.Status,
		"broadcast response must report status=published "+
			"(failure_reason=%q)", broadcastResp.FailureReason)
	require.NotEmpty(t, broadcastResp.Txid,
		"broadcast response must include a sweep txid")
	require.Greater(t, broadcastResp.FeePaidSat, int64(0),
		"fee_paid_sat must be set on a published sweep")
	require.Equal(t, int64(boardingAmount), broadcastResp.TotalAmountSat,
		"published sweep must aggregate the funded boarding amount")
	sweepTxID := broadcastResp.Txid
	feePaid := broadcastResp.FeePaidSat
	t.Logf("Broadcast sweep txid=%s fee_paid=%d", sweepTxID, feePaid)

	// Wait for the broadcast tx to land in the regtest mempool, then
	// confirm it with a few blocks.
	h.WaitMempoolTx(sweepTxID)
	h.GenerateAndWait(3)

	// The aggregate sweep row must transition to "confirmed" once the
	// chain backend reports every input spent. Per-input status is
	// "spent" when the spending txid matches the daemon-built sweep tx
	// (see client/db/boarding_sweep_store.go: MarkBoardingSweepInputSpent
	// flips to "external_spent" only when a different tx spent the
	// input). That is the happy-path we expect here regardless of the
	// destination address being external to the wallet.
	confirmedSweep := waitForBoardingSweepConfirmed(
		t, alice.RPCClient, sweepTxID,
	)
	require.Equal(t, externalAddr.String(),
		confirmedSweep.DestinationAddress,
		"persisted sweep must record the caller-supplied destination")
	require.Equal(t, int64(boardingAmount), confirmedSweep.TotalAmountSat,
		"persisted sweep total must match the boarding amount")
	require.Equal(t, feePaid, confirmedSweep.FeePaidSat,
		"persisted fee must match the broadcast response")
	require.Len(t, confirmedSweep.Inputs, 1,
		"sweep must track the single boarding input")
	require.Equal(t, "spent", confirmedSweep.Inputs[0].Status,
		"per-input status must be spent (sweep tx == spending tx)")
	require.Equal(t, sweepTxID, confirmedSweep.Inputs[0].SpentByTxid,
		"per-input spent_by_txid must point at the sweep txid")
	require.Equal(t, boardingOutpoint,
		confirmedSweep.Inputs[0].Outpoint,
		"per-input outpoint must match the funded boarding UTXO")

	// The accounting view must show that the boarding funds have left
	// the pending-sweep bucket and landed in the cumulative swept
	// bucket. BoardingSweptSat is gross (sum of swept boarding-input
	// amounts) per handleGetBoardingBalance — the miner fee is booked
	// separately under the onchain_fees account.
	finalBalance := waitForBoardingSweptBalance(
		t, alice.RPCClient, int64(boardingAmount),
	)
	require.Zero(t, finalBalance.BoardingConfirmedSat,
		"confirmed boarding balance must drain to zero on sweep")
	require.Zero(t, finalBalance.BoardingPendingSweepSat,
		"pending-sweep balance must drain to zero on confirmation")

	// The external destination is the operator's LND wallet, so the
	// swept output must surface in its confirmed UTXO set.
	sweptUTXO := waitForOperatorWalletUTXOAt(
		t, h, sweepTxID, externalPkScript,
	)
	require.NotNil(t, sweptUTXO)
	require.Equal(t, sweepTxID, sweptUTXO.OutPoint.Hash.String())
	// Expected swept value is boarding amount minus the miner fee AND
	// the above-dust anchor value carved off the parent. PR #355's
	// signBoardingSweepTx pays an above-dust P2A anchor (330 sats per
	// boardingSweepAnchorValue in client/wallet/boarding_sweep.go) so
	// the parent does not trip bitcoind's BIP-433 "dust must be 0-fee"
	// rule; that anchor reduces the destination output by 330 sats.
	const boardingSweepAnchorValue = int64(330)
	require.Equal(t,
		int64(boardingAmount)-feePaid-boardingSweepAnchorValue,
		int64(sweptUTXO.Value),
		"operator UTXO value must equal boarding amount minus fee "+
			"and anchor")
	require.True(t, bytes.Equal(sweptUTXO.PkScript, externalPkScript),
		"operator UTXO pkScript must match the external address")

	// The ledger must record exactly one boarding_sweep_fee_paid entry
	// (PR #356). FeeTypeOnchainSweep is booked as debit onchain_fees,
	// credit wallet_balance per the FeePaidMsg handler. The entry
	// carries no RoundID because sweep fees are not associated with a
	// round; dedup is supplied by the sweep txid as IdempotencyKey,
	// which the RPC response surface does not expose directly.
	feeEntry := waitForBoardingSweepFeeEntry(t, alice.RPCClient)
	require.Equal(t, feePaid, feeEntry.AmountSat,
		"ledger fee amount must equal the published fee_paid_sat")
	require.Equal(t, "onchain_fees", feeEntry.DebitAccount,
		"sweep fee must debit onchain_fees")
	require.Equal(t, "wallet_balance", feeEntry.CreditAccount,
		"sweep fee must credit wallet_balance")
	require.Empty(t, feeEntry.RoundId,
		"sweep fee entry must not carry a round_id")
	require.Empty(t, feeEntry.SessionId,
		"sweep fee entry must not carry a session_id")
}
