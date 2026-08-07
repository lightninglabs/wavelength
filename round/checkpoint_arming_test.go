package round

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newArmingCommitmentTx builds the smallest commitment packet the outbox
// helper will accept. The arming invariant does not care what the round is
// paying, only that the checkpoint was reached, so an empty unsigned tx is
// enough: TxHash works on it and confirmationWatchScript returns nil rather
// than panicking on the missing outputs.
func newArmingCommitmentTx() *psbt.Packet {
	return &psbt.Packet{
		UnsignedTx: wire.NewMsgTx(2),
	}
}

// newArmingEnv builds an environment carrying the given reconcile timeout.
// OperatorTerms has to be non-nil: the outbox reads MinConfirmations off it
// for the confirmation registration, so the bare struct literal the other
// reconcile tests use would panic here.
func newArmingEnv(reconcile time.Duration) *ClientEnvironment {
	base := reconcileEnv()
	base.StatusReconcileTimeout = reconcile
	base.OperatorTerms = &types.OperatorTerms{
		MinConfirmations: 1,
	}

	return base
}

// InputSigSentState is the checkpointed point of no return: the client's
// signatures are conceptually gone, the operator may broadcast at any moment,
// and no further event is guaranteed to arrive. The status-reconcile clock is
// the only thing that turns operator silence back into progress, so every
// door into that state has to arm it. Miss one and the rounds that walk
// through it strand until the CSV expires, which is wavelength#1051.
//
// There are two live doors plus the restart reload, and they are easy to
// treat as one: a refresh round with forfeits goes through forfeit
// collection, while a boarding-only round skips that entirely and checkpoints
// straight from PartialSigsSentState. The second door is the one that went
// unarmed, and no per-door test noticed because the boarding harness left
// StatusReconcileTimeout at zero, which skips the arming branch outright.
//
// So the tests below assert the invariant rather than the doors: whatever
// path reaches the checkpoint must emit the arm, and it must emit it ahead of
// the requests that can fail.

// findStartReconcile returns the status-reconcile StartTimeoutReq in an
// outbox along with its index, so callers can assert both presence and
// ordering.
func findStartReconcile(msgs []ClientOutMsg) (*StartTimeoutReq, int) {
	for i, msg := range msgs {
		req, ok := msg.(*StartTimeoutReq)
		if !ok {
			continue
		}
		if req.Phase == TimeoutPhaseStatusReconcile {
			return req, i
		}
	}

	return nil, -1
}

// assertArmsReconcileFirst is the shared invariant: the outbox arms the
// status-reconcile clock, and does so before anything that can fail on the
// way out. processOutbox stops at the first failed Tell, so an arm sitting
// behind the sig and registration requests is an arm a single mailbox hiccup
// can skip, leaving the state committed with no clock behind it.
func assertArmsReconcileFirst(t *testing.T, msgs []ClientOutMsg,
	timeout time.Duration, roundID RoundID) {

	t.Helper()

	arm, idx := findStartReconcile(msgs)
	require.NotNilf(
		t, arm, "checkpoint outbox never arms the status-reconcile "+
			"clock: a round reaching InputSigSentState through "+
			"this door has no liveness timer, so operator "+
			"silence strands it until the CSV expires "+
			"(wavelength#1051)",
	)
	require.Equal(t, timeout, arm.Duration)
	require.Equal(t, RoundKeyStr(roundID.KeyString()), arm.RoundKey)

	for i, msg := range msgs[:idx] {
		switch msg.(type) {
		// A cancel of some other phase is bookkeeping on a timer that
		// is already running, so it may precede the arm.
		case *CancelTimeoutReq:
			continue

		default:
			t.Fatalf("checkpoint outbox arms the reconcile clock "+
				"at index %d, behind %T at index %d: a failed "+
				"Tell on that message commits the checkpoint "+
				"with no clock armed", idx, msg, i)
		}
	}
}

// TestForfeitDoorArmsReconcileClock covers the forfeit-bearing door: a
// refresh round leaves through forfeit collection, so the arm rides in that
// outbox.
func TestForfeitDoorArmsReconcileClock(t *testing.T) {
	t.Parallel()

	const timeout = time.Minute

	roundID := reconcileRoundID(0x11)
	state := &ForfeitSignaturesCollectingState{
		RoundID:      roundID,
		CommitmentTx: newArmingCommitmentTx(),
	}
	env := newArmingEnv(timeout)

	forfeits := map[wire.OutPoint]*types.ForfeitTxSig{
		reconcileOutpoint(0x22): {},
	}

	t.Run("with boarding sigs", func(t *testing.T) {
		t.Parallel()

		msgs := state.forfeitCollectionOutbox(
			env, forfeits, []*types.BoardingInputSignature{{}},
		)
		assertArmsReconcileFirst(t, msgs, timeout, roundID)
	})

	t.Run("without boarding sigs", func(t *testing.T) {
		t.Parallel()

		msgs := state.forfeitCollectionOutbox(env, forfeits, nil)
		assertArmsReconcileFirst(t, msgs, timeout, roundID)
	})
}

// TestBoardingDoorArmsReconcileClock covers the door that was missed: a
// boarding-only round carries no forfeit mappings, so it never enters forfeit
// collection and checkpoints straight out of PartialSigsSentState. Before the
// fix this outbox held only the sig and registration requests, and the
// deposit had no clock at all.
func TestBoardingDoorArmsReconcileClock(t *testing.T) {
	t.Parallel()

	const timeout = time.Minute

	h := newRealSigningTestHarness(t)

	intent := h.newTestBoardingIntentWithTapscript()
	vtxoReq := h.newTestVTXORequestForIntent(intent)
	vtxtTree := h.newTestVTXOTreeForIntents([]types.VTXORequest{vtxoReq})

	validSigs, err := h.generateValidTreeSignatures(vtxtTree)
	require.NoError(t, err)
	require.NotEmpty(t, validSigs)

	commitmentTx := h.newCommitmentTxForIntents(
		[]BoardingIntent{intent}, vtxtTree,
	)

	clientTrees := make(map[SignerKey]*tree.Tree)
	clientTrees[NewSignerKey(vtxoReq.SigningKey.PubKey)] = vtxtTree

	roundID := testRoundIDTr("round-boarding-arm-001")
	state := &PartialSigsSentState{
		RoundID:      roundID,
		CommitmentTx: commitmentTx,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		Intents: Intents{
			Boarding: []BoardingIntent{
				intent,
			},
			VTXOs: []types.VTXORequest{
				vtxoReq,
			},
		},
		ClientTrees: clientTrees,
		BoardingInputIndices: map[wire.OutPoint]int{
			intent.Outpoint: 0,
		},
		Musig2Sessions: make(map[SignerKey]*tree.SignerSession),
	}

	// The harness leaves the reconcile timeout at zero, which skips the
	// arming branch entirely. That default is exactly why every existing
	// boarding test read this outbox and saw nothing missing.
	h.env.StatusReconcileTimeout = timeout

	h.setupMockWalletForBoardingSigning()
	h.setupMockRoundStoreForCommit()
	h.withState(state)

	transition, err := h.sendEvent(&OperatorSigned{
		RoundID: roundID,
		AggSigs: validSigs,
	})
	require.NoError(t, err)
	require.NotNil(t, transition)

	// The door has to actually land on the checkpoint, otherwise the
	// arming assertion below would pass over a round that never reached
	// the hazard window at all.
	assertStateType[*InputSigSentState](h.boardingTestHarness)

	var msgs []ClientOutMsg
	transition.NewEvents.WhenSome(func(emitted ClientEmittedEvent) {
		msgs = emitted.Outbox
	})
	assertArmsReconcileFirst(t, msgs, timeout, roundID)
}

// TestReconcileArmingRespectsDisabled pins the opt-out on both doors: a zero
// timeout means the operator has turned the reconcile off, and neither door
// may arm a timer behind its back.
func TestReconcileArmingRespectsDisabled(t *testing.T) {
	t.Parallel()

	roundID := reconcileRoundID(0x33)
	state := &ForfeitSignaturesCollectingState{
		RoundID:      roundID,
		CommitmentTx: newArmingCommitmentTx(),
	}
	env := newArmingEnv(0)

	msgs := state.forfeitCollectionOutbox(
		env, map[wire.OutPoint]*types.ForfeitTxSig{
			reconcileOutpoint(0x44): {},
		}, []*types.BoardingInputSignature{{}},
	)

	arm, _ := findStartReconcile(msgs)
	require.Nil(
		t, arm,
		"reconcile disabled but the outbox armed the clock anyway",
	)
}

// TestRestartDoorArmsReconcileClock covers the third door: a checkpointed
// round reloaded from the store on startup. The two live doors build their
// outbox from the transition that carries them into the state, but nothing
// re-derives that outbox across a restart, so recoverActiveRounds has to arm
// the clock itself. A boarding-only round is exactly the case that used to be
// gated out here, which mattered doubly: while the live door was unarmed, the
// restart path was the only thing that could still rescue such a round, and
// it was gated on the same forfeit count.
func TestRestartDoorArmsReconcileClock(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)

	roundID := testRoundID("restart-door-arms")
	round := h.newTestRound(roundID)

	walletIntent := h.newTestBoardingIntent()
	intent, err := buildBoardingIntentFromWallet(walletIntent)
	require.NoError(t, err)

	// A boarding-only checkpoint: intents carry no forfeits at all, so the
	// old gate skipped this round on the way back up.
	h.roundStore.On(
		"ListActiveRounds", mock.Anything,
	).Return([]*Round{round}, nil)
	h.roundStore.On(
		"FetchState", mock.Anything, round.RoundID,
	).Return(
		round,
		&InputSigSentState{
			RoundID:      roundID,
			CommitmentTx: round.CommitmentTx.UnwrapOrFail(t),
			Intents: Intents{
				Boarding: []BoardingIntent{intent},
			},
		},
		nil,
	)

	require.NoError(t, h.start())

	h.timeoutActor.assertTimeoutScheduled(
		t,
		makeTimeoutID(
			RoundKeyStr(
				roundID.KeyString(),
			),
			TimeoutPhaseStatusReconcile,
		),
		defaultStatusReconcileTimeout,
	)
}
