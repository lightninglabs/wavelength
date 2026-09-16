package round

import (
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// LoadCheckpoint returns the latest committed checkpoint to the behavior.
func (s *turnTestDatabase) LoadCheckpoint(_ context.Context, _ string) (
	*actor.Checkpoint, error) {

	if s.checkpoint.ActorID == "" {
		return nil, nil
	}

	return &actor.Checkpoint{
		ActorID:   s.checkpoint.ActorID,
		StateType: s.checkpoint.StateType,
		StateData: s.checkpoint.StateData,
		Version:   s.checkpoint.Version,
	}, nil
}

// TestDurableBehaviorRollback verifies that a failed commit cannot expose a
// cancelled round to the next query. It exercises the real TxBehavior against
// the explicit transaction-contract test double.
func TestDurableBehaviorRollback(t *testing.T) {
	h := newActorTestHarness(t)
	key, err := NewTempRoundKey()
	require.NoError(t, err)
	keyString := RoundKeyStr(key.KeyString())
	round := &RoundFSM{
		Key: key, env: &ClientEnvironment{
			RoundKey: keyString,
		},
	}
	h.actor.installRoundState(t.Context(), round, &PendingRoundAssembly{})
	h.actor.rounds[keyString] = round
	snapshot, err := h.actor.encodeActorSnapshot(t.Context())
	require.NoError(t, err)
	envelope := &durableClientCheckpoint{Snapshot: snapshot}
	data, err := envelope.encode()
	require.NoError(t, err)
	exec := &turnTestExec{database: turnTestDatabase{
		checkpoint: actor.CheckpointParams{
			ActorID: "round", StateData: data, Version: 3,
		},
	}}
	behavior := &durableClientBehavior{
		owner: h.actor, actorID: "round",
		codec: newDurableClientCodec(),
	}
	query, err := durableClientIngress(&GetClientStateRequest{})
	require.NoError(t, err)
	result := behavior.Receive(t.Context(), query, exec)
	require.ErrorContains(t, result.Err(), "restart has not completed")

	// Embedded restart data is stale; the behavior must load the current
	// database checkpoint, not replace it with the envelope's contents.
	restart := &actor.RestartMessage{Checkpoint: fn.Some(actor.Checkpoint{
		ActorID: "round", StateData: []byte("stale"), Version: 1,
	})}
	result = behavior.Receive(t.Context(), restart, exec)
	require.NoError(t, result.Err())
	require.True(t, exec.acked)
	require.EqualValues(t, 4, behavior.version)
	require.Contains(t, h.actor.rounds, keyString)

	cancel, err := durableClientIngress(&CancelRoundRequest{
		RoundKey: fn.Some(keyString),
	})
	require.NoError(t, err)
	exec.acked = false
	exec.failCommit = true
	result = behavior.Receive(t.Context(), cancel, exec)
	require.ErrorContains(t, result.Err(), "checkpoint failed")
	require.False(t, exec.acked)
	require.True(t, behavior.reload)
	require.Empty(t, h.actor.rounds)
	staged, err := decodeDurableClientCheckpoint(
		exec.database.checkpoint.StateData, behavior.codec,
	)
	require.NoError(t, err)
	require.NotEmpty(t, staged.InFlight)

	exec.failCommit = false
	result = behavior.Receive(t.Context(), query, exec)
	response, err := result.Unpack()
	require.NoError(t, err)
	states, ok := response.(*GetClientStateResponse)
	require.True(t, ok)
	require.Contains(t, states.States, string(keyString))
	require.True(t, exec.acked)
	require.False(t, behavior.reload)
	require.Nil(t, h.actor.queueEffect)
	t.Cleanup(h.actor.rounds[keyString].FSM.Stop)
}

// TestDurableLegacyImport adopts relational checkpoints exactly until the
// first native commit succeeds. A retry cannot expose effects from the failed
// commit, and later restarts must not consult stale relational state.
func TestDurableLegacyImport(t *testing.T) {
	h := newActorTestHarness(t)
	id := testRoundID("native-legacy-import")
	legacy := h.newTestRound(id)
	state := &InputSigSentState{
		RoundID: id, CommitmentTx: legacy.CommitmentTx.UnwrapOrFail(t),
	}
	h.roundStore.On("ListActiveRounds", mock.Anything).
		Return([]*Round{legacy}, nil).Twice()
	h.roundStore.On("FetchState", mock.Anything, id).
		Return(legacy, state, nil).Twice()
	exec := &turnTestExec{failCommit: true}
	behavior := &durableClientBehavior{
		owner: h.actor, actorID: "round",
		codec: newDurableClientCodec(),
	}
	restart := &actor.RestartMessage{}
	result := behavior.Receive(t.Context(), restart, exec)
	require.ErrorContains(t, result.Err(), "checkpoint failed")
	require.Empty(t, exec.database.checkpoint.ActorID)
	require.Empty(t, exec.database.effects)
	require.False(t, exec.acked)

	exec.failCommit = false
	result = behavior.Receive(t.Context(), restart, exec)
	require.NoError(t, result.Err())
	require.True(t, exec.acked)
	require.NotEmpty(t, exec.database.effects)
	require.Equal(t, "client-round", exec.database.checkpoint.StateType)
	key := RoundKeyStr(id.KeyString())
	require.Contains(t, h.actor.rounds, key)
	require.Equal(
		t, key, h.actor.commitmentTxIndex[h.actor.rounds[key].TxID],
	)

	result = behavior.Receive(t.Context(), restart, exec)
	require.NoError(t, result.Err())
	require.Contains(t, h.actor.rounds, key)
	require.EqualValues(t, 2, behavior.version)
	h.roundStore.AssertExpectations(t)
	t.Cleanup(h.actor.rounds[key].FSM.Stop)
}
