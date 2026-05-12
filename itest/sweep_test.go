//go:build itest

package itest

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/harness"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// waitForSweepMempoolTx waits until a new mempool transaction spends the round
// commitment output. The sweep destination script is returned via the matched
// transaction rather than being pre-specified.
func waitForSweepMempoolTx(t *testing.T, h *harness.ArkHarness,
	knownTxIDs map[string]struct{},
	roundTxID string) (string, *wire.MsgTx) {

	t.Helper()

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err, "should create bitcoind RPC client")

	var (
		matchedTxID string
		matchedTx   *wire.MsgTx
	)
	require.Eventually(t, func() bool {
		for _, txID := range h.MempoolTxIDs() {
			if _, known := knownTxIDs[txID]; known {
				continue
			}

			hash, err := chainhash.NewHashFromStr(txID)
			if err != nil {
				continue
			}

			rawTx, err := rpcClient.GetRawTransaction(hash)
			if err != nil {
				continue
			}

			tx := rawTx.MsgTx()
			if len(tx.TxIn) != 1 || len(tx.TxOut) != 1 {
				continue
			}

			prevHash := tx.TxIn[0].PreviousOutPoint.Hash
			if prevHash.String() != roundTxID {
				continue
			}

			matchedTxID = txID
			matchedTx = tx

			return true
		}

		return false
	}, defaultTimeout, pollInterval,
		"never observed batch sweep tx in mempool")

	return matchedTxID, matchedTx
}

// waitForOperatorWalletUTXO waits until operator LND reports the expected
// confirmed output as a spendable wallet UTXO.
func waitForOperatorWalletUTXO(t *testing.T, h *harness.ArkHarness, txID string,
	expectedPkScript []byte) *lnwallet.Utxo {

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
			if utxo.OutPoint.Hash.String() != txID {
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
		"operator wallet never reported confirmed output")

	return matched
}

// waitForNoLiveServerVTXOs polls the operator admin VTXO list until no live
// VTXOs remain. This verifies the server marked swept VTXOs as spent.
func waitForNoLiveServerVTXOs(t *testing.T, h *harness.ArkHarness) {
	t.Helper()

	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := h.ArkAdminClient.ListVTXOs(
			ctx, &adminrpc.ListVTXOsRequest{
				StatusFilter: []adminrpc.VTXOStatus{
					adminrpc.VTXOStatus_VTXO_STATUS_LIVE,
				},
			},
		)
		if err != nil {
			return false
		}

		return len(resp.Vtxos) == 0
	}, defaultTimeout, pollInterval,
		"server still reports live VTXOs after batch sweep")
}

// TestSweepIntegrationExpiredBatchSweepsToOperatorWallet verifies the
// production operator wiring broadcasts a sweep transaction once a confirmed
// batch reaches its sweep delay and that the resulting output lands in the
// operator's wallet.
func TestSweepIntegrationExpiredBatchSweepsToOperatorWallet(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	const sweepDelay = uint32(150)
	const boardingAmount = btcutil.Amount(2_000_000)

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
		OperatorConfigMutator: func(cfg *darepo.Config) {
			cfg.Rounds.SweepDelay = sweepDelay
		},
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	alice := h.StartClientDaemon("alice")
	operatorInfo := getOperatorInfo(t, h)
	require.Equal(t, sweepDelay, operatorInfo.SweepDelay)

	existingRoundIDs := snapshotClientRoundIDs(t, alice.RPCClient)

	newAddrResp, err := alice.RPCClient.NewAddress(
		t.Context(), &daemonrpc.NewAddressRequest{},
	)
	require.NoError(t, err, "NewAddress RPC failed")
	require.NotEmpty(
		t, newAddrResp.Address, "boarding address should be set",
	)

	// Use an amount that remains sweepable even when LND returns a
	// conservative regtest fee estimate for the expiry sweep.
	fundingTxID := h.Faucet(newAddrResp.Address, boardingAmount)
	t.Logf("Funded boarding address via txid=%s", fundingTxID)

	h.Generate(int(operatorInfo.MinConfirmations) + 1)
	waitForConfirmedBoardingBalance(
		t, alice.RPCClient, int64(boardingAmount),
	)

	boardResp := waitForBoardRegistered(t, alice.RPCClient)
	require.Equal(t, "registered", boardResp.Status)

	joinedRound := waitForNewClientRoundState(
		t, alice.RPCClient, existingRoundIDs,
		daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	require.NotEmpty(t, joinedRound.RoundId)

	waitForNamedClientRoundState(
		t, alice.RPCClient, joinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	waitForPersistedClientRoundState(
		t, alice.RPCClient, joinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, joinedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)

	confirmedBlock := mineUntilOperatorRoundConfirmed(
		t, h, joinedRound.RoundId, broadcastRound.TxId,
	)

	waitForNamedClientRoundState(
		t, alice.RPCClient, joinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_CONFIRMED,
	)

	confirmedRound := waitForOperatorRoundStatus(
		t, h, joinedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
	)
	require.NotEmpty(t, confirmedRound.TxId)

	currentHeight := int64(h.BlockCount())
	expiryHeight := confirmedBlock.Header.Height + int64(sweepDelay)
	blocksToExpiry := int(expiryHeight - currentHeight)
	require.Greater(
		t, blocksToExpiry, 0, "batch should not already be expired",
	)

	knownMempool := make(map[string]struct{})
	for _, txID := range h.MempoolTxIDs() {
		knownMempool[txID] = struct{}{}
	}

	h.Generate(blocksToExpiry)

	sweepTxID, sweepTx := waitForSweepMempoolTx(
		t, h, knownMempool, confirmedRound.TxId,
	)
	require.Equal(t, 1, len(sweepTx.TxIn))
	require.Equal(t, 1, len(sweepTx.TxOut))
	require.Greater(t, sweepTx.TxOut[0].Value, int64(0))
	require.Equal(
		t, confirmedRound.TxId,
		sweepTx.TxIn[0].PreviousOutPoint.Hash.String(),
	)

	// Extract the actual sweep destination from the broadcast tx.
	sweepPkScript := sweepTx.TxOut[0].PkScript

	h.GenerateAndWait(3)

	sweptUTXO := waitForOperatorWalletUTXO(
		t, h, sweepTxID, sweepPkScript,
	)
	require.NotNil(t, sweptUTXO)
	require.Greater(t, int64(sweptUTXO.Value), int64(0))
	require.Equal(t, sweepTxID, sweptUTXO.OutPoint.Hash.String())
	require.True(t, bytes.Equal(
		sweptUTXO.PkScript, sweepPkScript,
	))

	// After the batch is swept, the server should have marked all VTXOs
	// from that batch as spent. Query the operator's admin VTXO list
	// and verify no live VTXOs remain.
	waitForNoLiveServerVTXOs(t, h)
}
