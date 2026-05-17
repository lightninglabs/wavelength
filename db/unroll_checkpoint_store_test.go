package db

import (
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

func TestUnrollCheckpointStoreSaveLoad(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	testDB := NewTestDB(t)
	store := NewStore(
		testDB.DB, testDB.Queries, testDB.Backend(), btclog.Disabled,
	)

	testClock := clock.NewTestClock(time.Unix(1234, 0))
	checkpoints := NewUnrollCheckpointStore(store, testClock)

	loaded, err := checkpoints.LoadCheckpoint(ctx, "unroll:target")
	require.NoError(t, err)
	require.Nil(t, loaded)

	err = checkpoints.SaveCheckpoint(ctx, actor.CheckpointParams{
		ActorID:   "unroll:target",
		StateType: "awaiting-confirmation",
		StateData: []byte{0x01, 0x02, 0x03},
		Version:   1,
	})
	require.NoError(t, err)

	loaded, err = checkpoints.LoadCheckpoint(ctx, "unroll:target")
	require.NoError(t, err)
	require.Equal(t, "unroll:target", loaded.ActorID)
	require.Equal(t, "awaiting-confirmation", loaded.StateType)
	require.Equal(t, []byte{0x01, 0x02, 0x03}, loaded.StateData)
	require.EqualValues(t, 1, loaded.Version)
	require.Equal(t, time.Unix(1234, 0), loaded.UpdatedAt)

	testClock.SetTime(time.Unix(5678, 0))
	err = checkpoints.SaveCheckpoint(ctx, actor.CheckpointParams{
		ActorID:   "unroll:target",
		StateType: "swept",
		StateData: []byte{0x04},
		Version:   2,
	})
	require.NoError(t, err)

	loaded, err = checkpoints.LoadCheckpoint(ctx, "unroll:target")
	require.NoError(t, err)
	require.Equal(t, "swept", loaded.StateType)
	require.Equal(t, []byte{0x04}, loaded.StateData)
	require.EqualValues(t, 2, loaded.Version)
	require.Equal(t, time.Unix(5678, 0), loaded.UpdatedAt)
}
