//go:build systest

package systest

import (
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	clientround "github.com/lightninglabs/darepo-client/round"
	"github.com/stretchr/testify/require"
)

// newSharedOperatorLNDClient creates a test client backed by the server's LND
// instance. This mirrors the shared-signer deployment that previously allowed
// the client VTXO signing key to collide with the operator key.
func newSharedOperatorLNDClient(t *testing.T, h *E2EHarness) *TestClient {
	t.Helper()

	backend := newLNDBackendFromInstance(h, h.serverLND)

	return NewTestClientWithExistingDB(
		h, backend,
		fmt.Sprintf(
			"%s/shared-operator.db", t.TempDir(),
		),
	)
}

// TestSharedOperatorSignerBoardingNoKeyCollision verifies that a client using
// the same LND signer as the Ark operator can still complete a boarding round
// without deriving the operator key as its VTXO signing key.
func TestSharedOperatorSignerBoardingNoKeyCollision(t *testing.T) {
	ParallelN(t)

	if *backendFlag != "lnd" {
		t.Skip(
			"shared LND signer regression only applies to LND " +
				"backend",
		)
	}

	h := NewE2EHarness(
		t, DisableFees(), WithFixedOperatorKeyLocator(),
	)
	h.Start()
	h.FundServerWallet(btcutil.SatoshiPerBitcoin)

	ctx := t.Context()
	client := newSharedOperatorLNDClient(t, h)
	terms := h.Terms()

	boardingResp, err := client.CreateBoardingAddress(
		terms.BoardingExitDelay,
	)
	require.NoError(t, err)

	amount := btcutil.Amount(100_000)
	h.Harness.Faucet(boardingResp.Address.String(), amount)
	h.MineBlocks(int(terms.MinBoardingConfirmations))

	err = client.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(t, err)

	err = client.RegisterVTXORequests(ctx, []btcutil.Amount{amount - 5000})
	require.NoError(t, err)

	err = client.TriggerRegistration(ctx)
	require.NoError(t, err)

	err = h.Transcript().WaitForEntryCount(
		msgsPerClientJoin, 10*time.Second,
	)
	require.NoError(t, err)
	h.Transcript().AssertContainsMessage(t, S2C("ClientSuccessResp"))

	signingKey := intentSentSigningKey(t, client)
	require.NotEqual(
		t, schnorr.SerializePubKey(h.operatorKeyDesc.PubKey),
		schnorr.SerializePubKey(signingKey),
		"client VTXO signing key must be disjoint from the "+
			"operator key when both use the same LND",
	)

	h.TriggerRoundSeal()

	err = h.Transcript().WaitForEntryCount(
		msgsPerClientRound, 30*time.Second,
	)
	require.NoError(t, err)
	h.Transcript().AssertNotContainsMessage(t, S2C("ClientRoundFailedResp"))

	// Wait for the finalized round transaction to reach bitcoind's
	// mempool before mining its confirmation block.
	time.Sleep(time.Second)

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)

	mempoolTxs, err := rpcClient.GetRawMempool()
	require.NoError(t, err)
	require.Len(t, mempoolTxs, 1)

	h.MineBlocksAndConfirm(1)

	err = client.WaitForRoundComplete(30 * time.Second)
	require.NoError(t, err)
}

// intentSentSigningKey returns the VTXO signing key from the client's
// IntentSentState after it has registered for a round.
func intentSentSigningKey(t *testing.T, client *TestClient) *btcec.PublicKey {
	t.Helper()

	future := client.roundRef.Ask(
		client.harness.ctx, &clientround.GetClientStateRequest{},
	)
	result := future.Await(client.harness.ctx)
	require.False(t, result.IsErr(), "get client state: %v", result.Err())

	respVal, _ := result.Unpack()
	resp, ok := respVal.(*clientround.GetClientStateResponse)
	require.True(t, ok, "unexpected state response: %T", respVal)

	for _, fsmState := range resp.States {
		state, ok := fsmState.State.(*clientround.IntentSentState)
		if !ok {
			continue
		}

		require.Len(t, state.Intents.VTXOs, 1)

		return state.Intents.VTXOs[0].SigningKey.PubKey
	}

	require.FailNow(t, "client did not reach IntentSentState")

	return nil
}
