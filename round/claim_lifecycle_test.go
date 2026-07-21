package round

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
)

// TestClaimReplacementOutpointsAndFinalization verifies the signed client tree
// fixes the replacement outpoint and the confirmation hook receives that exact
// source-to-replacement pair once the new VTXO is locally durable.
func TestClaimReplacementOutpointsAndFinalization(t *testing.T) {
	t.Parallel()

	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	replacement := mkReq(t, operator.PubKey(), 41, true)
	source := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("expired-claim-source")),
		Index: 3,
	}
	claim := VTXOClaimIntent{
		Input: types.VTXOClaimInput{
			SourceOutpoint: source,
		},
		ExpectedOutput: replacement.req,
	}

	mappings := claimReplacementOutpoints(
		t.Context(), btclog.Disabled, []VTXOClaimIntent{claim},
		map[SignerKey]*tree.Tree{
			NewSignerKey(replacement.req.SigningKey.PubKey): replacement.tree,
		},
	)
	expected, err := replacement.tree.Root.GetLeafNodes()[0].
		GetNonAnchorOutpoint()
	require.NoError(t, err)
	require.Equal(t, *expected, mappings[source])

	var (
		finalizedSource      wire.OutPoint
		finalizedReplacement wire.OutPoint
		finalizedCalls       int
	)
	actor := &RoundClientActor{
		cfg: &RoundClientConfig{
			FinalizeVTXOClaim: func(_ context.Context,
				gotSource, gotReplacement wire.OutPoint) error {

				finalizedCalls++
				finalizedSource = gotSource
				finalizedReplacement = gotReplacement

				return nil
			},
		},
		log: btclog.Disabled,
	}
	roundFSM := &RoundFSM{
		ClaimIntents: []VTXOClaimIntent{
			claim,
		},
		ClaimReplacements: mappings,
	}
	actor.finalizeRoundClaims(t.Context(), roundFSM, []*ClientVTXO{{
		Outpoint: *expected,
	}})

	require.Equal(t, 1, finalizedCalls)
	require.Equal(t, source, finalizedSource)
	require.Equal(t, *expected, finalizedReplacement)
	require.Empty(t, roundFSM.ClaimReplacements)

	// Replaying the same creation notification is idempotent at the actor
	// boundary after the successful mapping has been removed.
	actor.finalizeRoundClaims(t.Context(), roundFSM, []*ClientVTXO{{
		Outpoint: *expected,
	}})
	require.Equal(t, 1, finalizedCalls)
}

// TestRoundJoinedMarksClaimWithAssignedRound verifies a fresh next-open-round
// submission is adopted under the concrete UUID returned by the operator.
func TestRoundJoinedMarksClaimWithAssignedRound(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	tempKey := h.setupRoundInIntentSentState()
	roundFSM := h.actor.rounds[RoundKeyStr(tempKey.KeyString())]
	source := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("admitted-claim-source")),
		Index: 4,
	}
	claim := VTXOClaimIntent{
		Input: types.VTXOClaimInput{
			SourceOutpoint: source,
		},
	}
	roundFSM.ClaimIntents = []VTXOClaimIntent{claim}
	state, err := roundFSM.FSM.CurrentState()
	require.NoError(t, err)
	intentState, ok := state.(*IntentSentState)
	require.True(t, ok)
	intentState.Intents.Claims = []VTXOClaimIntent{claim}

	var (
		markedRoundID string
		markedSources []wire.OutPoint
	)
	h.actor.cfg.MarkVTXOClaimsRedeeming = func(_ context.Context,
		roundID string, sources []wire.OutPoint) error {

		markedRoundID = roundID
		markedSources = append([]wire.OutPoint(nil), sources...)

		return nil
	}
	assigned := testRoundID("admitted-claim-round")
	result := h.actor.handleRoundJoined(t.Context(), &RoundJoined{
		RoundID:               assigned,
		AcceptedVTXOOutpoints: []wire.OutPoint{source},
	})
	_, err = result.Unpack()
	require.NoError(t, err)
	require.Equal(t, assigned.String(), markedRoundID)
	require.Equal(t, []wire.OutPoint{source}, markedSources)
	require.Same(
		t, roundFSM, h.actor.rounds[RoundKeyStr(assigned.KeyString())],
	)
}

// TestRoundJoinedMarkFailureRevertsAssignedClaim verifies an admission
// persistence failure fails the local round and rolls back using the concrete
// round UUID, so a stale failure cannot release a later claim attempt.
func TestRoundJoinedMarkFailureRevertsAssignedClaim(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	tempKey := h.setupRoundInIntentSentState()
	roundFSM := h.actor.rounds[RoundKeyStr(tempKey.KeyString())]
	source := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("failed-admission-claim-source")),
		Index: 5,
	}
	claim := VTXOClaimIntent{
		Input: types.VTXOClaimInput{
			SourceOutpoint: source,
		},
	}
	roundFSM.ClaimIntents = []VTXOClaimIntent{claim}
	state, err := roundFSM.FSM.CurrentState()
	require.NoError(t, err)
	intentState, ok := state.(*IntentSentState)
	require.True(t, ok)
	intentState.Intents.Claims = []VTXOClaimIntent{claim}

	h.actor.cfg.MarkVTXOClaimsRedeeming = func(context.Context, string,
		[]wire.OutPoint) error {

		return errors.New("disk unavailable")
	}
	var (
		revertedRoundID string
		revertedSources []wire.OutPoint
	)
	h.actor.cfg.RevertVTXOClaimsRedeeming = func(_ context.Context,
		roundID string, sources []wire.OutPoint) error {

		revertedRoundID = roundID
		revertedSources = append([]wire.OutPoint(nil), sources...)

		return nil
	}

	assigned := testRoundID("failed-admission-claim-round")
	result := h.actor.handleRoundJoined(t.Context(), &RoundJoined{
		RoundID:               assigned,
		AcceptedVTXOOutpoints: []wire.OutPoint{source},
	})
	_, err = result.Unpack()
	require.ErrorContains(t, err, "disk unavailable")
	require.Equal(t, assigned.String(), revertedRoundID)
	require.Equal(t, []wire.OutPoint{source}, revertedSources)

	state, err = roundFSM.FSM.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &ClientFailedState{}, state)
}
