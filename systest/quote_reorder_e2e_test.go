//go:build systest

package systest

import (
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/stretchr/testify/require"
)

// TestRoundJoinAckSurvivesTransientSendFailure exercises the durable
// mailbox per-correlation-key FIFO invariant end-to-end. The scenario:
//
//  1. Client joins a round. The server's per-client durable mailbox
//     enqueues ClientSuccessResp (the join-ack).
//  2. The InstrumentedMailbox fault-injects exactly one failure for
//     the first ClientSuccessResp Send. The server's per-client
//     egress actor nacks-with-backoff (default ~1s).
//  3. While ClientSuccessResp is in retry backoff, the round seals
//     and the server emits JoinRoundQuote into the same per-client
//     mailbox. JoinRoundQuote's available_at is strictly smaller
//     than the in-backoff ClientSuccessResp's.
//  4. Without the per-key FIFO claim invariant, the actor would
//     claim JoinRoundQuote first and the client would observe the
//     quote before the join-ack (forcing the defensive
//     bufferPendingQuote path on the client).
//  5. With the per-key FIFO claim invariant, the actor refuses to
//     claim JoinRoundQuote until ClientSuccessResp's backoff
//     expires and its retry succeeds. The transcript therefore
//     records ClientSuccessResp before JoinRoundQuote.
func TestRoundJoinAckSurvivesTransientSendFailure(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := t.Context()

	h.FundServerWallet(btcutil.SatoshiPerBitcoin)

	client := NewTestClient(h)
	require.NotNil(t, client)

	terms := h.Terms()
	boardingResp, err := client.CreateBoardingAddress(
		terms.BoardingExitDelay,
	)
	require.NoError(t, err)

	amount := btcutil.Amount(100_000)
	h.Harness.Faucet(boardingResp.Address.String(), amount)

	h.MineBlocks(int(terms.MinBoardingConfirmations))

	require.NoError(t, client.WaitForBoardingConfirmation(30*time.Second))

	vtxoAmount := amount - 5000
	require.NoError(
		t,
		client.RegisterVTXORequests(
			ctx, []btcutil.Amount{vtxoAmount},
		),
	)

	// Arm the fault-injection rule BEFORE triggering registration: the
	// first server-to-client ClientSuccessResp Send to this client
	// will return a transient error so the durable-mailbox egress
	// actor nacks it and backs off.
	transientErr := errors.New("transient send failure injected by test " +
		"harness")
	h.Bridge().FailNextSends(
		ServerToClient, client.ClientID(),
		"ClientSuccessResp", 1, transientErr,
	)

	require.NoError(t, client.TriggerRegistration(ctx))

	// Wait for the join-request to leave the client. The fault-
	// injection rule swallows the first ClientSuccessResp on the
	// server side, so only JoinRoundRequest is observable in the
	// transcript at this point.
	require.NoError(
		t, h.Transcript().WaitForEntryCount(1, 5*time.Second),
		"client should send JoinRoundRequest",
	)
	h.Transcript().AssertContainsMessage(t, C2S("JoinRoundRequest"))

	// Trigger the round seal while ClientSuccessResp is still in
	// retry backoff. The server emits JoinRoundQuote into the
	// per-client mailbox. With the per-key FIFO fix, the actor
	// refuses to claim JoinRoundQuote until ClientSuccessResp is
	// drained first.
	h.TriggerRoundSeal()

	// Drive the round to completion. We need both ClientSuccessResp
	// (after retry) and JoinRoundQuote (after CSR drains) to land,
	// the client to accept the quote, the server to build the batch,
	// MuSig2 signing to complete, and the boarding signatures to
	// be submitted.
	require.NoError(
		t, h.Transcript().WaitForEntryCount(
			msgsPerClientRound, 30*time.Second,
		),
		"the round must complete despite the transient send failure",
	)

	// Assert the critical ordering property: in the recorded
	// transcript (which only records successful Sends), the S2C
	// ClientSuccessResp appears before the S2C JoinRoundQuote. With
	// the per-key FIFO claim invariant, the in-backoff
	// ClientSuccessResp blocks the same-key JoinRoundQuote on the
	// server side until the retry of CSR succeeds, after which the
	// quote is claim-eligible.
	entries := h.Transcript().Entries()

	var (
		ackIdx   = -1
		quoteIdx = -1
	)
	for i, entry := range entries {
		if entry.Direction != ServerToClient {
			continue
		}
		if entry.ClientID != client.ClientID() {
			continue
		}
		switch entry.MsgType {
		case "ClientSuccessResp":
			if ackIdx == -1 {
				ackIdx = i
			}

		case "JoinRoundQuote":
			if quoteIdx == -1 {
				quoteIdx = i
			}
		}
	}

	require.NotEqual(
		t, -1, ackIdx,
		"transcript must record the join-ack successful delivery",
	)
	require.NotEqual(
		t, -1, quoteIdx,
		"transcript must record the join quote successful delivery",
	)
	require.Less(
		t, ackIdx, quoteIdx, "per-key FIFO must hold "+
			"ClientSuccessResp ahead of JoinRoundQuote even "+
			"when CSR transiently fails",
	)

	// Drive the round through commitment broadcast + confirmation so
	// the harness teardown is clean and the test exercises the full
	// pipeline including the post-FIFO phases.
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		mempoolTxs, err := rpcClient.GetRawMempool()
		if err != nil {
			return false
		}

		return len(mempoolTxs) == 1
	}, 15*time.Second, 250*time.Millisecond,
		"commitment tx should reach the mempool")

	h.MineBlocksAndConfirm(1)

	require.NoError(t, client.WaitForRoundComplete(30*time.Second))
	client.AssertConfirmedRoundCountFromDB(1)
	client.AssertVTXOCountFromDB(1)
}
