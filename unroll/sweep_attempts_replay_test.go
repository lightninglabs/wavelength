package unroll

import (
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// TestUnrollSweepAttemptsOverCountUnderReplayIsBounded is a characterization
// test that pins the KNOWN, documented over-count of the sweep retry budget
// under a Stage-then-lost-Commit replay. See the replay caveat on
// applySweepBuildFailed: SweepAttempts++ is the one non-monotone FSM mutation,
// so a failure message that is Staged but whose Commit loses its lease is
// redelivered and re-applied, advancing the counter twice for one logical
// failure.
//
// This is a bounded, no-fund-loss divergence (the sweep is never
// double-broadcast; only the counter inflates, terminating a job at most
// maxSweepAttempts retries early). The test asserts the current behavior so the
// divergence is on record and any future change to it is caught; the fully
// idempotent retry accounting is tracked as a follow-up FSM change. If that
// follow-up lands, the final assertion should flip from 2 to 1.
func TestUnrollSweepAttemptsOverCountUnderReplayIsBounded(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())

	// Drive a real harness all the way to a broadcast sweep so the behavior
	// holds a started FSM session in AwaitingSweepConfirmation with
	// b.sweepTx set and Sweep.Txid recorded.
	unrollActor, b, txconfirmRef, _, _ := newActorHarnessExec(
		t, proof, desc,
	)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})
	txconfirmRef.emitConfirmed(t, 0, proof.RootTxids()[0], 101)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(
			proof.TargetOutpoint().Hash,
		) == 1
	}, testTimeout, 10*time.Millisecond)
	txconfirmRef.emitConfirmed(t, 1, proof.TargetOutpoint().Hash, 102)
	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 104})
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() == 3
	}, testTimeout, 10*time.Millisecond)

	// Capture the broadcast sweep txid and confirm the budget is untouched.
	sweepTxid := txconfirmRef.lastRequest(t).Tx.TxHash()
	require.Equal(t, 0, currentSweepAttempts(t, b))

	// Build a TxFailedMsg for the sweep txid -- exactly what txconfirm
	// would deliver on a mempool rejection / fee-spike eviction. This is
	// the SINGLE logical failure we will see replayed.
	failMsg := &TxFailedMsg{
		Txid:   sweepTxid,
		Reason: "sweep rejected (fee too low)",
	}

	// First delivery: the consume Commit loses its lease. Stage writes
	// (including the SweepAttempts++ inside applySweepBuildFailed) already
	// ran and are durable; the message is NOT acked, so the framework
	// redelivers it.
	exec1 := newMemExecFor(b)
	exec1.commitLeaseLost.Store(true)
	res1 := b.Receive(t.Context(), failMsg, exec1)
	require.True(t, res1.IsErr())
	require.ErrorIs(t, res1.Err(), actor.ErrLeaseLost)

	attemptsAfterFirst := currentSweepAttempts(t, b)
	require.Equal(
		t, 1, attemptsAfterFirst,
		"first delivery records exactly one sweep attempt",
	)

	// Redelivery of the SAME message (lease reacquired, Commit now
	// succeeds). The replay re-applies the failure against the already
	// advanced state because applySweepBuildFailed reset the sweep back to
	// Pending under the same txid, so the txid still matches and the
	// counter advances again.
	exec2 := newMemExecFor(b)
	res2 := b.Receive(t.Context(), failMsg, exec2)
	require.True(t, res2.IsOk(), "redelivery should consume cleanly")

	attemptsAfterReplay := currentSweepAttempts(t, b)

	// Known bounded behavior: a single logical failure advanced the retry
	// budget by two. When the idempotent-retry follow-up lands this becomes
	// 1.
	require.Equal(
		t, 2, attemptsAfterReplay, "known bounded over-count: one "+
			"logical sweep failure advances the retry budget "+
			"twice under lease-lost replay",
	)
}

// currentSweepAttempts reads the SweepAttempts counter off the behavior's
// current FSM state.
func currentSweepAttempts(t *testing.T, b *behavior) int {
	t.Helper()

	state, err := b.currentState()
	require.NoError(t, err)

	return stateJob(state).SweepAttempts
}
