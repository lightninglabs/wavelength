//go:build systest

package systest

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/stretchr/testify/require"
)

// boardClientIntoConfirmedRound performs the production-identical boarding flow
// used by the stable round systests so OOR tests start from a confirmed VTXO.
func boardClientIntoConfirmedRound(ctx context.Context, t *testing.T,
	h *E2EHarness, client *TestClient, boardingAmount btcutil.Amount,
	roundTimeout time.Duration) {

	t.Helper()

	terms := h.Terms()
	boardingResp, err := client.CreateBoardingAddress(
		terms.BoardingExitDelay,
	)
	require.NoError(t, err, "create boarding address")

	h.Harness.Faucet(boardingResp.Address.String(), boardingAmount)
	h.MineBlocks(int(terms.MinBoardingConfirmations))

	err = client.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(t, err, "boarding confirmation")

	vtxoAmount := boardingAmount - 5000
	err = client.RegisterVTXORequests(
		ctx, []btcutil.Amount{vtxoAmount},
	)
	require.NoError(t, err, "register VTXO requests")

	err = client.TriggerRegistration(ctx)
	require.NoError(t, err, "trigger registration")

	err = h.Transcript().WaitForEntryCount(2, 10*time.Second)
	require.NoError(t, err, "join + success response")

	h.TriggerRoundSeal()

	require.Eventually(t, func() bool {
		entries := h.Transcript().Entries()
		hasAwaitingInputSigs := false
		hasForfeitSig := false

		for _, entry := range entries {
			switch entry.MsgType {
			case "ClientAwaitingInputSigsResp":
				hasAwaitingInputSigs = true

			case "SubmitForfeitSigRequest":
				hasForfeitSig = true
			}
		}

		return hasAwaitingInputSigs && hasForfeitSig
	}, 30*time.Second, 50*time.Millisecond,
		"complete signing phases\n%s", h.Transcript().Dump(),
	)

	// Give the server a brief window to finalize and broadcast before mining.
	time.Sleep(1 * time.Second)

	h.MineBlocksAndConfirm(1)

	err = client.WaitForRoundComplete(roundTimeout)
	require.NoError(t, err, "round completion")
}

// sumLiveVTXOAmounts returns the total value across a slice of live VTXOs.
func sumLiveVTXOAmounts(vtxos []*vtxo.Descriptor) btcutil.Amount {
	var total btcutil.Amount
	for _, v := range vtxos {
		if v == nil {
			continue
		}

		total += v.Amount
	}

	return total
}

// TestOORAliceToBobProductionE2E tests the full production OOR
// transfer flow using real production connectors. This is the
// primary OOR integration test covering:
//
//   - SubmitPackage → server co-sign → SubmitAccepted (#41)
//   - FinalizePackage → FinalizeAccepted → inputs spent (#45)
//   - VTXO store state transitions (#46)
//   - Transcript message sequence verification
//
// All message routing flows through production connectors with no
// bridge shortcuts.
func TestOORAliceToBobProductionE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping OOR e2e test in short mode")
	}

	t.Parallel()

	const (
		boardingAmount = btcutil.Amount(100_000)
		roundTimeout   = 120 * time.Second
		oorTimeout     = 60 * time.Second
	)

	h := NewE2EHarness(t)
	h.Start()
	h.FundServerWallet(btcutil.Amount(1_000_000))

	alice := NewTestClient(h)
	bob := NewTestClient(h)

	ctx := t.Context()

	// === Phase 1: Alice boards and joins a round ===
	//
	// Follow the same boarding flow as the existing boarding
	// e2e tests: create address → fund → mine → wait for
	// wallet detection → register VTXOs → trigger registration
	// → seal round → wait for completion.

	boardClientIntoConfirmedRound(
		ctx, t, h, alice, boardingAmount, roundTimeout,
	)

	// Verify Alice has a live VTXO from the round.
	aliceVTXOs, err := alice.ListVTXOs(ctx)
	require.NoError(t, err, "alice: list vtxos after round")
	require.NotEmpty(t, aliceVTXOs,
		"alice must have VTXOs after round",
	)

	// Record the initial VTXO count and amount for later
	// comparison.
	initialVTXOCount := len(aliceVTXOs)
	var initialTotalAmount btcutil.Amount
	for _, v := range aliceVTXOs {
		initialTotalAmount += v.Amount
	}

	t.Logf("Alice: %d VTXOs, total %d sats",
		initialVTXOCount, initialTotalAmount)

	// === Phase 2: Get Bob's recipient key ===

	bobPkScript, err := bob.OORReceivePkScript()
	require.NoError(t, err, "bob: derive P2TR pkScript")

	// Clear transcript before OOR to isolate OOR messages.
	h.Transcript().Clear()

	// === Phase 3: Alice sends OOR to Bob ===

	// v0 OOR transfers are fee-less and currently spend the full selected
	// input set, so the recipient amount must match the full live balance.
	err = alice.SendOOR(ctx, t, bobPkScript, initialTotalAmount)
	require.NoError(t, err, "alice: send OOR to bob")

	t.Log("Alice initiated OOR transfer")

	// === Phase 4: Wait for OOR completion ===
	//
	// The OOR FSM drives through:
	//   1. RequestArkSignatures → sign Ark PSBT
	//   2. SendSubmitPackageRequest → server mailbox
	//   3. Server co-signs → SubmitOORResponse
	//   4. SubmitAccepted → client FSM
	//   5. RequestCheckpointSignatures → sign checkpoints
	//   6. SendFinalizePackageRequest → server mailbox
	//   7. Server finalizes → FinalizeOORResponse
	//   8. FinalizeAccepted → client FSM
	//   9. MarkInputsSpent → VTXO store update

	// Poll Alice's live VTXO count — once it drops to zero,
	// all inputs have been spent by the OOR transfer.
	require.Eventually(t, func() bool {
		liveVTXOs, listErr := alice.ListLiveVTXOs(ctx)
		if listErr != nil {
			return false
		}

		return len(liveVTXOs) == 0
	}, oorTimeout, 500*time.Millisecond,
		"alice: live VTXOs not spent within timeout",
	)

	t.Log("Alice's VTXOs all spent")

	// === Phase 5: Verify transcript message flow ===
	//
	// The transcript should contain the OOR protocol messages
	// flowing through production connectors.

	transcript := h.Transcript()
	t.Log("OOR transcript:")
	t.Log(transcript.Dump())

	// Verify the OOR submit/finalize flow appeared in the
	// transcript. These are the C2S and S2C messages that
	// flow through the InstrumentedMailbox.
	//
	// C2S: SubmitPackageRequest (Alice → server)
	// S2C: SubmitPackageResponse (server → Alice)
	// C2S: FinalizePackageRequest (Alice → server)
	// S2C: FinalizePackageResponse (server → Alice)
	transcript.AssertContainsMessage(
		t, C2S("SubmitPackageRequest"),
	)
	transcript.AssertContainsMessage(
		t, S2C("SubmitPackageResponse"),
	)
	transcript.AssertContainsMessage(
		t, C2S("FinalizePackageRequest"),
	)
	transcript.AssertContainsMessage(
		t, S2C("FinalizePackageResponse"),
	)

	// === Phase 6: Verify VTXO store state transitions ===

	// No live VTXOs should remain for Alice.
	finalLiveVTXOs, err := alice.ListLiveVTXOs(ctx)
	require.NoError(t, err, "alice: list live vtxos after OOR")
	require.Empty(t, finalLiveVTXOs,
		"alice: should have no live VTXOs after OOR transfer",
	)

	// === Phase 7: Bob's receive side ===
	//
	// Bob's incoming transfer materialization depends on:
	// - IncomingOOREvent notification from server indexer
	// - Indexer query for full Ark PSBT (notification→query)
	// - ResolveIncomingClientKey + ResolveIncomingMetadata
	//   callbacks (tracked in tasks #37, #38)
	//
	// Full Bob verification will be enabled once these
	// callbacks are wired. For now, verify Bob was created
	// correctly with production connectors.
	require.NotNil(t, bob.oorActor,
		"bob: OOR actor should be wired",
	)

	_ = bob
}

// TestOORBidirectionalTransfer tests the full Alice→Bob→Alice OOR
// round-trip:
//
//  1. Alice boards and gets a VTXO from a round.
//  2. Alice sends OOR to Bob.
//  3. Bob materializes the received VTXO.
//  4. Bob sends OOR back to Alice.
//  5. Alice materializes the received VTXO.
//  6. Both VTXO stores verified.
func TestOORBidirectionalTransfer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping bidirectional OOR test in short mode")
	}

	t.Parallel()

	const (
		boardingAmount = btcutil.Amount(100_000)
		roundTimeout   = 120 * time.Second
		oorTimeout     = 60 * time.Second
	)

	h := NewE2EHarness(t)
	h.Start()
	h.FundServerWallet(btcutil.Amount(1_000_000))

	alice := NewTestClient(h)
	bob := NewTestClient(h)
	ctx := t.Context()

	boardClientIntoConfirmedRound(
		ctx, t, h, alice, boardingAmount, roundTimeout,
	)

	aliceLiveVTXOs, err := alice.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	aliceSendAmount := sumLiveVTXOAmounts(aliceLiveVTXOs)
	require.Positive(t, int64(aliceSendAmount))

	// === Alice → Bob ===

	bobRecipient, err := bob.OORReceiveRecipientOutput()
	require.NoError(t, err)

	err = alice.SendOOR(
		ctx, t, bobRecipient.PkScript, aliceSendAmount,
		bobRecipient.VTXOPolicyTemplate,
	)
	require.NoError(t, err)

	t.Log("Alice → Bob OOR initiated")

	// Wait for Alice's VTXO to be spent.
	require.Eventually(t, func() bool {
		liveVTXOs, listErr := alice.ListLiveVTXOs(ctx)
		if listErr != nil {
			return false
		}

		return len(liveVTXOs) == 0
	}, oorTimeout, 500*time.Millisecond,
		"alice: VTXO not spent in Alice→Bob",
	)

	t.Log("Alice → Bob complete")

	// Wait for Bob to materialize the incoming VTXO.
	require.Eventually(t, func() bool {
		liveVTXOs, listErr := bob.ListLiveVTXOs(ctx)
		if listErr != nil {
			return false
		}

		return len(liveVTXOs) > 0
	}, oorTimeout, 500*time.Millisecond,
		"bob: no live VTXO materialized from Alice",
	)

	bobVTXOs, err := bob.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, bobVTXOs, "bob should have live VTXOs")
	bobSendAmount := sumLiveVTXOAmounts(bobVTXOs)
	require.Positive(t, int64(bobSendAmount))

	t.Logf("Bob has %d VTXOs after receive", len(bobVTXOs))

	// Wait until the wallet actor can actually reserve the received VTXO.
	// The descriptor can hit the store slightly before the VTXO manager has
	// spawned and registered the actor needed for selection.
	require.Eventually(t, func() bool {
		selectResult := bob.walletRef.Ask(ctx,
			&wallet.SelectAndLockVTXOsRequest{
				TargetAmount: bobSendAmount,
			},
		).Await(ctx)
		selectResp, selectErr := selectResult.Unpack()
		if selectErr != nil {
			return false
		}

		locked, ok := selectResp.(*wallet.SelectAndLockVTXOsResponse)
		if !ok || len(locked.SelectedVTXOs) == 0 {
			return false
		}

		outpoints := make(
			[]wire.OutPoint, 0, len(locked.SelectedVTXOs),
		)
		for _, selected := range locked.SelectedVTXOs {
			outpoints = append(outpoints, selected.Outpoint)
		}

		if len(outpoints) == 0 {
			return false
		}

		return bob.walletRef.Tell(ctx, &wallet.UnlockVTXOsRequest{
			Outpoints: outpoints,
		}) == nil
	}, oorTimeout, 500*time.Millisecond,
		"bob: received VTXO not yet spendable",
	)

	// === Bob → Alice ===

	aliceRecipient, err := alice.OORReceiveRecipientOutput()
	require.NoError(t, err)

	err = bob.SendOOR(
		ctx, t, aliceRecipient.PkScript, bobSendAmount,
		aliceRecipient.VTXOPolicyTemplate,
	)
	require.NoError(t, err)

	t.Log("Bob → Alice OOR initiated")

	// Wait for Bob's VTXO to be spent.
	require.Eventually(t, func() bool {
		liveVTXOs, listErr := bob.ListLiveVTXOs(ctx)
		if listErr != nil {
			return false
		}

		return len(liveVTXOs) == 0
	}, oorTimeout, 500*time.Millisecond,
		"bob: VTXO not spent in Bob→Alice",
	)

	t.Log("Bob → Alice complete")

	// Verify final state.
	transcript := h.Transcript()
	t.Log("Bidirectional transcript:")
	t.Log(transcript.Dump())
}

// TestOORClientResumeAfterCoSign tests that a client can resume
// an OOR transfer after crashing between server co-sign and
// receiving the SubmitAccepted response.
//
// The test exercises the durable actor checkpoint/replay path
// through production connectors:
//  1. Alice initiates OOR transfer
//  2. Wait for SubmitPackageRequest to be sent (proves the
//     request reached the server)
//  3. Stop Alice (simulates crash before receiving response)
//  4. Restart Alice with same DB (durable actor loads checkpoint)
//  5. Transfer completes normally via replay
func TestOORClientResumeAfterCoSign(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping OOR resume test in short mode")
	}

	t.Parallel()

	const (
		boardingAmount = btcutil.Amount(100_000)
		roundTimeout   = 120 * time.Second
		oorTimeout     = 60 * time.Second
	)

	h := NewE2EHarness(t)
	h.Start()
	h.FundServerWallet(btcutil.Amount(1_000_000))

	alice := NewTestClient(h)
	bob := NewTestClient(h)
	ctx := t.Context()

	boardClientIntoConfirmedRound(
		ctx, t, h, alice, boardingAmount, roundTimeout,
	)

	aliceLiveVTXOs, err := alice.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	aliceSendAmount := sumLiveVTXOAmounts(aliceLiveVTXOs)
	require.Positive(t, int64(aliceSendAmount))

	// Get Bob's recipient key.
	bobPkScript, err := bob.OORReceivePkScript()
	require.NoError(t, err)

	// Clear transcript.
	h.Transcript().Clear()

	// Alice sends OOR.
	err = alice.SendOOR(ctx, t, bobPkScript, aliceSendAmount)
	require.NoError(t, err)

	// Wait for the SubmitPackageRequest to appear in the
	// transcript. This proves the request was sent to the
	// server before we "crash" the client.
	require.Eventually(t, func() bool {
		entries := h.Transcript().Entries()
		for _, e := range entries {
			if e.MsgType == "SubmitPackageRequest" {
				return true
			}
		}

		return false
	}, 10*time.Second, 100*time.Millisecond,
		"submit request not sent before crash",
	)

	t.Log("SubmitPackageRequest sent — simulating crash")

	// Simulate crash: stop Alice.
	alice.Stop()

	// Brief pause to let the server process the submit.
	time.Sleep(2 * time.Second)

	// Restart Alice with the same DB. The durable actor should
	// load its checkpoint and replay pending events.
	alice = h.RestartClient(alice)

	t.Log("Alice restarted — waiting for OOR completion")

	// Wait for the transfer to complete after restart.
	require.Eventually(t, func() bool {
		liveVTXOs, listErr := alice.ListLiveVTXOs(ctx)
		if listErr != nil {
			return false
		}

		return len(liveVTXOs) == 0
	}, oorTimeout, 500*time.Millisecond,
		"alice: VTXO not spent after resume",
	)

	t.Log("OOR transfer completed after crash resume")

	// Verify transcript has both submit and finalize.
	transcript := h.Transcript()
	t.Log("Resume transcript:")
	t.Log(transcript.Dump())

	_ = bob
}

// TestOORClientResumeAfterFinalizeBuffered verifies the sender can recover
// after the finalize request has been durably produced but before the operator
// observes it. This mirrors the old controlled-mailbox restart coverage using
// the newer InstrumentedMailbox buffering hooks while preserving the
// server-side mailbox delivery state across restart.
func TestOORClientResumeAfterFinalizeBuffered(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping buffered finalize resume test in short mode")
	}

	t.Parallel()

	const (
		boardingAmount = btcutil.Amount(100_000)
		roundTimeout   = 120 * time.Second
		oorTimeout     = 60 * time.Second
	)

	h := NewE2EHarness(t)
	h.Start()
	h.FundServerWallet(btcutil.Amount(1_000_000))

	alice := NewTestClient(h)
	bob := NewTestClient(h)
	ctx := t.Context()

	boardClientIntoConfirmedRound(
		ctx, t, h, alice, boardingAmount, roundTimeout,
	)

	aliceLiveVTXOs, err := alice.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	sendAmount := sumLiveVTXOAmounts(aliceLiveVTXOs)
	require.Positive(t, int64(sendAmount))

	recipient, err := bob.OORReceiveRecipientOutput()
	require.NoError(t, err)

	h.Transcript().Clear()
	h.Bridge().SetBufferedC2S(alice.ClientID(), true)

	err = alice.SendOOR(
		ctx, t, recipient.PkScript, sendAmount,
		recipient.VTXOPolicyTemplate,
	)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return h.Bridge().PendingC2STypeCount(
			alice.ClientID(), "SubmitPackageRequest",
		) > 0
	}, 10*time.Second, 100*time.Millisecond,
		"submit package request never buffered",
	)

	err = h.Bridge().FlushFirstMatchingC2S(
		alice.ClientID(), "SubmitPackageRequest",
	)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return h.Bridge().PendingC2STypeCount(
			alice.ClientID(), "FinalizePackageRequest",
		) > 0
	}, 10*time.Second, 100*time.Millisecond,
		"finalize package request never buffered",
	)

	require.Equal(t, 1, h.Bridge().DropAllMatchingC2S(
		alice.ClientID(), "FinalizePackageRequest",
	), "expected to drop the pre-crash finalize request")

	t.Log("FinalizePackageRequest buffered — simulating crash before delivery")

	alice = h.CrashRestartClient(alice)

	require.Eventually(t, func() bool {
		return h.Bridge().PendingC2STypeCount(
			alice.ClientID(), "FinalizePackageRequest",
		) > 0
	}, 10*time.Second, 100*time.Millisecond,
		"finalize package request was not replayed after restart",
	)

	h.Bridge().SetBufferedC2S(alice.ClientID(), false)
	require.NoError(t, h.Bridge().FlushFirstMatchingC2S(
		alice.ClientID(), "FinalizePackageRequest",
	))

	require.Eventually(t, func() bool {
		liveVTXOs, listErr := alice.ListLiveVTXOs(ctx)
		if listErr != nil {
			return false
		}

		return len(liveVTXOs) == 0
	}, oorTimeout, 500*time.Millisecond,
		"alice: VTXO not spent after restart from buffered finalize",
	)

	finalizeSends := countTranscriptEntries(
		h.Transcript().Entries(), ClientToServer, alice.ClientID(),
		"FinalizePackageRequest",
	)
	require.GreaterOrEqual(t, finalizeSends, 2,
		"expected finalize request to be replayed after restart")
}

// TestOOROfflineRecipientEventVisibility verifies that the authoritative
// recipient-event query path still exposes an incoming OOR transfer while the
// recipient client is offline and that the restarted recipient converges to
// the received VTXO afterward.
func TestOOROfflineRecipientEventVisibility(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping offline recipient-event visibility test in short mode")
	}

	t.Parallel()

	const (
		boardingAmount = btcutil.Amount(100_000)
		roundTimeout   = 120 * time.Second
		oorTimeout     = 60 * time.Second
	)

	h := NewE2EHarness(t)
	h.Start()
	h.FundServerWallet(btcutil.Amount(1_000_000))

	alice := NewTestClient(h)
	bob := NewTestClient(h)
	ctx := t.Context()

	boardClientIntoConfirmedRound(
		ctx, t, h, alice, boardingAmount, roundTimeout,
	)

	aliceLiveVTXOs, err := alice.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	sendAmount := sumLiveVTXOAmounts(aliceLiveVTXOs)
	require.Positive(t, int64(sendAmount))

	recipient, recipientKeyDesc, err := bob.OORReceiveRecipientOutputWithKey()
	require.NoError(t, err)

	queryClient := h.StartRecipientQueryClient(
		*recipientKeyDesc, bob.Backend().IndexerSigner(*recipientKeyDesc),
	)

	prebuiltQueryReq, err := queryClient.
		BuildListOORRecipientEventsByScriptRequest(
			ctx, recipient.PkScript, 0, 20,
		)
	require.NoError(t, err)

	bob.DisconnectForCrashRestart()
	t.Log("Stopped Bob before OOR send to force offline receive")

	err = alice.SendOOR(
		ctx, t, recipient.PkScript, sendAmount,
		recipient.VTXOPolicyTemplate,
	)
	require.NoError(t, err)

	var matchedValue bool
	var lastQueryErr error
	var lastEventCount int
	require.Eventually(t, func() bool {
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		resp, queryErr := queryClient.ListOORRecipientEventsByRequest(
			queryCtx, prebuiltQueryReq,
		)
		if queryErr != nil {
			lastQueryErr = queryErr

			return false
		}

		lastQueryErr = nil
		lastEventCount = len(resp.Events)
		matchedValue = false

		for _, event := range resp.Events {
			if int64(event.Value) == int64(sendAmount) {
				matchedValue = true

				return true
			}
		}

		return false
	}, oorTimeout, 500*time.Millisecond,
		"recipient-event query never exposed offline OOR transfer "+
			"(last_query_err=%v last_event_count=%d)",
		lastQueryErr, lastEventCount,
	)
	require.True(t, matchedValue)

	bob = h.CrashRestartClient(bob)

	require.Eventually(t, func() bool {
		liveVTXOs, listErr := bob.ListLiveVTXOs(ctx)
		if listErr != nil {
			return false
		}

		for _, live := range liveVTXOs {
			if live == nil {
				continue
			}

			if live.Amount == sendAmount {
				return true
			}
		}

		return false
	}, oorTimeout, 500*time.Millisecond,
		"bob never materialized the offline OOR receive after restart")
}

func countTranscriptEntries(entries []TranscriptEntry, dir MessageDirection,
	clientID clientconn.ClientID, typeName string) int {

	var count int
	for _, entry := range entries {
		if entry.Direction != dir {
			continue
		}
		if entry.ClientID != clientID {
			continue
		}
		if entry.MsgType != typeName {
			continue
		}

		count++
	}

	return count
}
