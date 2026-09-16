package db

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/stretchr/testify/require"
)

// TestServiceOperationOwnership persists cold deferral and rejects a second
// operation or changed input set while the original ownership remains active.
func TestServiceOperationOwnership(t *testing.T) {
	database := NewTestDB(t)
	store := NewStore(
		database.DB, database.Queries, database.Backend(),
		btclog.Disabled,
	)
	journal := store.NewServiceOperationStore()
	input := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 2}
	op := round.DeferredServiceOperation{
		Service: types.ServiceRequest{
			OperationID: [32]byte{
				1,
			}, Mode: types.ServiceScheduled,
			ScheduleVersion: 1, ExpiresAtUnix: 1000,
			AllowFallback: true,
		}, Inputs: []wire.OutPoint{
			input,
		},
	}
	require.NoError(t, journal.SaveServiceOperation(t.Context(), op))
	owner := testRoundIDDB("deferred-service")
	require.NoError(
		t,
		journal.BindServiceOperation(
			t.Context(), op.Service.OperationID, owner,
			time.Unix(100, 0),
		),
	)
	// A fresh store object reads the persisted record, not an in-memory
	// copy.
	journal = store.NewServiceOperationStore()
	deferred, err := journal.ListDeferredServiceOperations(t.Context())
	require.NoError(t, err)
	require.Len(t, deferred, 1)
	require.Equal(t, owner, deferred[0].RoundID)
	require.Equal(t, op.Service, deferred[0].Service)
	require.Equal(t, op.Inputs, deferred[0].Inputs)
	held, err := journal.HasServiceInputOwner(t.Context(), input)
	require.NoError(t, err)
	require.True(t, held)
	other := op
	other.Service.OperationID[0] = 2
	require.ErrorContains(
		t,
		journal.SaveServiceOperation(
			t.Context(), other,
		),
		"deferred",
	)
	changed := op
	changed.Inputs = []wire.OutPoint{{Hash: chainhash.Hash{3}}}
	require.ErrorContains(
		t,
		journal.SaveServiceOperation(
			t.Context(), changed,
		),
		"input set",
	)
	changed = op
	changed.Service.FeeLimitSat = 1
	require.ErrorContains(
		t,
		journal.SaveServiceOperation(
			t.Context(), changed,
		),
		"authorization",
	)
	// Fallback keeps the same inputs, cap, and expiry, clearing the old
	// round.
	op.Service.Mode = types.ServiceImmediate
	op.Service.ScheduleVersion = 0
	require.NoError(t, journal.SaveServiceOperation(t.Context(), op))
	deferred, err = journal.ListDeferredServiceOperations(t.Context())
	require.NoError(t, err)
	require.Len(t, deferred, 1)
	require.Zero(t, deferred[0].RoundID)
	require.Equal(t, op.Service, deferred[0].Service)
	require.NoError(
		t,
		journal.CompleteServiceOperation(
			t.Context(), op.Service.OperationID,
		),
	)
	held, err = journal.HasServiceInputOwner(t.Context(), input)
	require.NoError(t, err)
	require.False(t, held)
}
