package chainresolver

import (
	"encoding/json"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/stretchr/testify/require"
)

// TestDeserializeResolverState_BroadcastingTree verifies correct deserialization
// of the broadcasting_tree state from JSON.
func TestDeserializeResolverState_BroadcastingTree(t *testing.T) {
	t.Parallel()

	outpoint := testOutpoint(1)

	original := &BroadcastingTreeState{
		Outpoint:            outpoint,
		Trigger:             ResolveTriggerExpiry,
		CurrentLevel:        1,
		MaxLevel:            3,
		ConfirmedLevels:     1,
		AlreadyOnChainLevel: 0,
	}

	details, err := json.Marshal(original)
	require.NoError(t, err)

	state, err := deserializeResolverState(
		outpoint, "broadcasting_tree", details,
	)
	require.NoError(t, err)

	treeState, ok := state.(*BroadcastingTreeState)
	require.True(t, ok, "expected *BroadcastingTreeState, got %T", state)
	require.Equal(t, 1, treeState.CurrentLevel)
	require.Equal(t, 3, treeState.MaxLevel)
	require.Equal(t, 1, treeState.ConfirmedLevels)
	require.Equal(t, 0, treeState.AlreadyOnChainLevel)
}

// TestDeserializeResolverState_BroadcastingCheckpoints verifies correct
// deserialization of the broadcasting_checkpoints state from JSON.
func TestDeserializeResolverState_BroadcastingCheckpoints(t *testing.T) {
	t.Parallel()

	outpoint := testOutpoint(2)

	original := &BroadcastingCheckpointsState{
		Outpoint: outpoint,
		Packages: []*db.OORPackageBundle{
			{SessionID: chainhash.Hash{1}},
			{SessionID: chainhash.Hash{2}},
		},
		CurrentPackageIdx: 1,
		CSVDelay:          144,
		LastConfHeight:    800000,
		WaitingForCSV:     true,
	}

	details, err := json.Marshal(original)
	require.NoError(t, err)

	state, err := deserializeResolverState(
		outpoint, "broadcasting_checkpoints", details,
	)
	require.NoError(t, err)

	cpState, ok := state.(*BroadcastingCheckpointsState)
	require.True(t, ok,
		"expected *BroadcastingCheckpointsState, got %T", state)
	require.Equal(t, 1, cpState.CurrentPackageIdx)
	require.Equal(t, uint32(144), cpState.CSVDelay)
	require.Equal(t, int32(800000), cpState.LastConfHeight)
	require.True(t, cpState.WaitingForCSV)
	require.Len(t, cpState.Packages, 2)
}

// TestDeserializeResolverState_WatchingCommitment verifies deserialization
// of the watching_commitment state. This state carries no JSON details.
func TestDeserializeResolverState_WatchingCommitment(t *testing.T) {
	t.Parallel()

	outpoint := testOutpoint(3)

	state, err := deserializeResolverState(
		outpoint, "watching_commitment", nil,
	)
	require.NoError(t, err)

	watchState, ok := state.(*WatchingCommitmentState)
	require.True(t, ok,
		"expected *WatchingCommitmentState, got %T", state)
	require.Equal(t, ResolveTriggerFraudReactive, watchState.Trigger)
	require.Equal(t, outpoint, watchState.Outpoint)
}

// TestDeserializeResolverState_Resolved verifies deserialization of the
// resolved terminal state.
func TestDeserializeResolverState_Resolved(t *testing.T) {
	t.Parallel()

	outpoint := testOutpoint(4)

	state, err := deserializeResolverState(
		outpoint, "resolved", nil,
	)
	require.NoError(t, err)

	resolved, ok := state.(*ResolvedState)
	require.True(t, ok, "expected *ResolvedState, got %T", state)
	require.Equal(t, outpoint, resolved.Outpoint)
	require.True(t, resolved.IsTerminal())
}

// TestDeserializeResolverState_Failed verifies deserialization of the
// failed terminal state.
func TestDeserializeResolverState_Failed(t *testing.T) {
	t.Parallel()

	outpoint := testOutpoint(5)

	state, err := deserializeResolverState(
		outpoint, "failed", nil,
	)
	require.NoError(t, err)

	failed, ok := state.(*FailedState)
	require.True(t, ok, "expected *FailedState, got %T", state)
	require.Equal(t, outpoint, failed.Outpoint)
	require.Equal(t, "recovered from storage", failed.Reason)
	require.True(t, failed.IsTerminal())
}

// TestDeserializeResolverState_Unknown verifies that an unknown state name
// returns an error.
func TestDeserializeResolverState_Unknown(t *testing.T) {
	t.Parallel()

	_, err := deserializeResolverState(
		testOutpoint(6), "banana", nil,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown state")
}

// TestDeserializeResolverState_InvalidJSON verifies that invalid JSON details
// return a parse error.
func TestDeserializeResolverState_InvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := deserializeResolverState(
		testOutpoint(7), "broadcasting_tree", []byte("not json"),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unmarshal")
}

// TestNewCoordinator verifies that the constructor initializes the coordinator
// with an empty resolver map.
func TestNewCoordinator(t *testing.T) {
	t.Parallel()

	coord := NewCoordinator(&CoordinatorConfig{})

	require.NotNil(t, coord)
	require.NotNil(t, coord.resolvers)
	require.Len(t, coord.resolvers, 0)
}

// TestTerminalStateName verifies the terminal state name helper.
func TestTerminalStateName(t *testing.T) {
	t.Parallel()

	require.Equal(t, "resolved", terminalStateName(true))
	require.Equal(t, "failed", terminalStateName(false))
}

// TestCompletionOutbox verifies the outbox messages emitted for terminal
// states.
func TestCompletionOutbox(t *testing.T) {
	t.Parallel()

	outpoint := testOutpoint(10)
	finalOutpoint := testOutpoint(20)

	// Success case.
	outbox := completionOutbox(outpoint, finalOutpoint, true, "done")
	require.Len(t, outbox, 2)

	statusMsg, ok := outbox[0].(*ResolverStatusUpdateOutMsg)
	require.True(t, ok)
	require.Equal(t, "resolved", statusMsg.StateName)
	require.Equal(t, outpoint, statusMsg.Outpoint)

	completedMsg, ok := outbox[1].(*ResolverCompletedOutMsg)
	require.True(t, ok)
	require.True(t, completedMsg.Success)
	require.Equal(t, outpoint, completedMsg.Outpoint)
	require.Equal(t, finalOutpoint, completedMsg.FinalOutpoint)
	require.Equal(t, "done", completedMsg.Reason)

	// Failure case.
	outbox = completionOutbox(
		outpoint, wire.OutPoint{}, false, "broke",
	)
	require.Len(t, outbox, 2)

	statusMsg, ok = outbox[0].(*ResolverStatusUpdateOutMsg)
	require.True(t, ok)
	require.Equal(t, "failed", statusMsg.StateName)

	completedMsg, ok = outbox[1].(*ResolverCompletedOutMsg)
	require.True(t, ok)
	require.False(t, completedMsg.Success)
}

// TestBuildBatchSpendWatch verifies spend watch construction for fraud-reactive
// monitoring.
func TestBuildBatchSpendWatch(t *testing.T) {
	t.Parallel()

	resolverID := testOutpoint(15)
	batchOutpoint := testOutpoint(16)
	pkScript := []byte{0x51, 0x20, 0x01}

	watch := buildBatchSpendWatch(
		resolverID, batchOutpoint, pkScript, 800000,
	)

	require.Equal(t, batchOutpoint, watch.Outpoint)
	require.Equal(t, pkScript, watch.PkScript)
	require.Equal(t, uint32(800000), watch.HeightHint)
	require.Contains(t, watch.CallerID, resolverID.String())
}
