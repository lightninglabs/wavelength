package round

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
)

// committedAuthorization records the gate's nonce-boundary call.
type committedAuthorization struct {
	request RoundReadinessRequest
	token   []byte
}

// blockingReadinessGate exposes asynchronous preparation to tests.
type blockingReadinessGate struct {
	started   chan RoundReadinessRequest
	release   chan struct{}
	committed chan committedAuthorization
	token     []byte
	awaitErr  error
	commitErr error
}

// AwaitSigningAuthorization implements RoundReadinessGate.
func (g *blockingReadinessGate) AwaitSigningAuthorization(ctx context.Context,
	request RoundReadinessRequest) ([]byte, error) {

	select {
	case g.started <- request.Clone():
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case <-g.release:
		return append([]byte(nil), g.token...), g.awaitErr

	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// CommitSigningAuthorization records the durable nonce boundary.
func (g *blockingReadinessGate) CommitSigningAuthorization(ctx context.Context,
	request RoundReadinessRequest, token []byte) error {

	if g.committed == nil {
		return g.commitErr
	}

	select {
	case g.committed <- committedAuthorization{
		request: request.Clone(),
		token:   append([]byte(nil), token...),
	}:
		return g.commitErr

	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestRoundReadinessGatePreventsNonceGeneration verifies a configured gate
// parks the FSM only after exact output validation and resumes explicitly.
func TestRoundReadinessGatePreventsNonceGeneration(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.env.ReadinessGate = &blockingReadinessGate{}

	intent := h.newTestBoardingIntent()
	vtxoRequest := h.newTestVTXORequestForIntent(intent)
	vtxoTree := h.newTestVTXOTreeForIntents(
		[]types.VTXORequest{vtxoRequest},
	)
	commitmentTx := h.bindTreeToCommitment(
		[]BoardingIntent{intent}, vtxoTree,
	)
	roundID := testRoundIDTr("readiness-round")
	h.withState(&CommitmentTxReceivedState{
		RoundID:      roundID,
		CommitmentTx: commitmentTx,
		TxID:         commitmentTx.UnsignedTx.TxHash(),
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxoTree,
		},
		Intents: Intents{
			Boarding: []BoardingIntent{intent},
			VTXOs:    []types.VTXORequest{vtxoRequest},
		},
		ClientTrees: make(map[SignerKey]*tree.Tree),
		SweepDelay:  1008,
	})

	transition, err := h.sendEvent(&CommitmentTxBuilt{
		RoundID: roundID,
		Tx:      commitmentTx,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxoTree,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, transition)

	pending := assertStateType[*RoundReadinessPendingState](h)
	require.NotNil(t, pending.Validated)
	require.Len(t, h.outboxMessages, 1)
	await, ok := h.outboxMessages[0].(*AwaitRoundReadinessRequest)
	require.True(t, ok)
	require.Equal(t, roundID, await.Request.RoundID)
	require.Equal(
		t, commitmentTx.UnsignedTx.TxHash(),
		await.Request.CommitmentTxID,
	)
	require.Len(t, await.Request.Outputs, 1)
	require.Equal(
		t, vtxoRequest.SigningKey.PubKey.SerializeCompressed(),
		await.Request.Outputs[0].SigningKey[:],
	)

	h.clearOutbox()
	token := []byte("durable-channel-token")
	transition, err = h.sendEvent(&RoundReadinessResolved{
		RoundID: roundID,
		Token:   token,
	})
	require.NoError(t, err)
	validated := assertStateType[*CommitmentTxValidatedState](h)
	require.NotNil(t, validated.ReadinessRequest)
	require.Equal(t, token, validated.ReadinessToken)
	assertTransitionEmitsInternalEvent[*GenerateNonces](h, transition)
}

// TestRoundReadinessFailureStopsBeforeSigning verifies preparation failure
// releases the round before any nonce or forfeit signature.
func TestRoundReadinessFailureStopsBeforeSigning(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	roundID := testRoundIDTr("readiness-failed")
	validated := h.newCommitmentTxValidatedState(
		roundID, []BoardingIntent{h.newTestBoardingIntent()},
	)
	h.withState(&RoundReadinessPendingState{
		Validated: validated,
		Request: RoundReadinessRequest{
			RoundID: roundID,
		},
	})

	_, err := h.sendEvent(&RoundReadinessResolved{
		RoundID: roundID,
		Err:     errors.New("peer did not acknowledge backing"),
	})
	require.NoError(t, err)
	failed := assertStateType[*ClientFailedState](h)
	require.Contains(t, failed.Reason, "readiness")
	h.assertOutboxContainsType("RoundFailedNotification")
}

// TestRoundReadinessCommitPrecedesNonce verifies a failed durable commit
// reaches the failed state before the signer creates or sends a nonce.
func TestRoundReadinessCommitPrecedesNonce(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	roundID := testRoundIDTr("readiness-commit")
	request := RoundReadinessRequest{RoundID: roundID}
	commitErr := errors.New("channel revision conflict")
	committed := make(chan committedAuthorization, 1)
	h.env.ReadinessGate = &blockingReadinessGate{
		committed: committed,
		commitErr: commitErr,
	}
	validated := h.newCommitmentTxValidatedState(
		roundID, []BoardingIntent{h.newTestBoardingIntent()},
	)
	validated.ReadinessRequest = &request
	validated.ReadinessToken = []byte("channel-token")
	h.withState(validated)

	_, err := h.sendEvent(&GenerateNonces{})
	require.NoError(t, err)
	failed := assertStateType[*ClientFailedState](h)
	require.Contains(t, failed.Reason, "authorization")

	commit := <-committed
	require.Equal(t, roundID, commit.request.RoundID)
	require.Equal(t, []byte("channel-token"), commit.token)
}

// TestRoundReadinessWorkerIsNonBlocking verifies slow preparation runs outside
// processOutbox and returns through the actor's self reference.
func TestRoundReadinessWorkerIsNonBlocking(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	gate := &blockingReadinessGate{
		started: make(chan RoundReadinessRequest, 1),
		release: make(chan struct{}),
		token:   []byte("ready"),
	}
	h.actor.cfg.ReadinessGate = gate

	roundID := testRoundIDTr("actor-readiness")
	request := RoundReadinessRequest{RoundID: roundID}
	err := h.actor.processOutbox(h.ctx, []ClientOutMsg{
		&AwaitRoundReadinessRequest{Request: request},
	})
	require.NoError(t, err)

	select {
	case received := <-gate.started:
		require.True(t, sameRoundReadinessRequest(request, received))

	case <-time.After(time.Second):
		t.Fatal("readiness worker did not start")
	}

	select {
	case <-h.selfRef.msgChan:
		t.Fatal("readiness completed before gate release")

	default:
	}

	close(gate.release)
	message, ok := h.selfRef.waitForMessage(time.Second)
	require.True(t, ok)
	resolved, ok := message.(*RoundReadinessResolved)
	require.True(t, ok)
	require.Equal(t, roundID, resolved.RoundID)
	require.Equal(t, []byte("ready"), resolved.Token)
}

// sameRoundReadinessRequest compares exact validated output context.
func sameRoundReadinessRequest(a, b RoundReadinessRequest) bool {
	if a.RoundID != b.RoundID || a.CommitmentTxID != b.CommitmentTxID ||
		len(a.Outputs) != len(b.Outputs) {
		return false
	}
	for i := range a.Outputs {
		left := a.Outputs[i]
		right := b.Outputs[i]
		if left.SigningKey != right.SigningKey ||
			left.VTXOOutpoint != right.VTXOOutpoint ||
			left.Amount != right.Amount ||
			!bytes.Equal(
				left.PolicyTemplate, right.PolicyTemplate,
			) ||
			!bytes.Equal(left.PkScript, right.PkScript) ||
			left.TreePath != right.TreePath {
			return false
		}
	}

	return true
}

var _ RoundReadinessGate = (*blockingReadinessGate)(nil)
