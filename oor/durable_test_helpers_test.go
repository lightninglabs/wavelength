package oor

import (
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newActorDeliveryStoreForTest creates an actor delivery store against the
// provided DB handle for durability tests.
func newActorDeliveryStoreForTest(t testing.TB,
	dbq db.BatchedQuerier) actor.DeliveryStore {

	t.Helper()

	store, err := db.NewActorDeliveryStoreFromDB(
		dbq, clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	return store
}
