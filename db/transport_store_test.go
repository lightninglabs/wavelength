package db

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

func newTransportStoreForTest(t testing.TB,
	clk clock.Clock) (*TransportStoreDB, *Store) {

	t.Helper()

	sqlDB := NewTestDB(t)
	store := NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	)

	return NewTransportStore(store, clk), store
}

func TestTransportStoreEgressClaimExpiresAndReclaims(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	transport, _ := newTransportStoreForTest(t, clk)

	row := serverconn.EgressEnvelope{
		ID:              "egress-1",
		Connector:       serverconn.TransportConnectorServerConn,
		LocalMailboxID:  "client-local",
		RemoteMailboxID: "server-remote",
		RPCKind:         "event",
		Service:         "round.v1.RoundService",
		Method:          "AwaitingInputSigs",
		MsgID:           "msg-1",
		IdempotencyKey:  "idem-1",
		Envelope:        []byte("serialized-envelope"),
	}
	require.NoError(t, transport.InsertEgress(ctx, row))

	claimed, err := transport.ClaimDueEgress(
		ctx, "worker-a", 10, time.Minute,
	)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, row.ID, claimed[0].ID)
	require.Equal(t, row.Envelope, claimed[0].Envelope)
	require.Equal(t, int32(1), claimed[0].Attempts)
	require.NotEmpty(t, claimed[0].ClaimToken)
	firstToken := claimed[0].ClaimToken

	claimedAgain, err := transport.ClaimDueEgress(
		ctx, "worker-b", 10, time.Minute,
	)
	require.NoError(t, err)
	require.Empty(t, claimedAgain)

	clk.SetTime(clk.Now().Add(time.Minute + time.Second))
	require.NoError(t, transport.ReleaseExpiredEgressClaims(ctx))

	reclaimed, err := transport.ClaimDueEgress(
		ctx, "worker-b", 10, time.Minute,
	)
	require.NoError(t, err)
	require.Len(t, reclaimed, 1)
	require.Equal(t, row.ID, reclaimed[0].ID)
	require.Equal(t, int32(2), reclaimed[0].Attempts)
	require.NotEqual(t, firstToken, reclaimed[0].ClaimToken)

	require.NoError(
		t, transport.MarkEgressSent(
			ctx, reclaimed[0].ID, reclaimed[0].ClaimToken,
		),
	)

	clk.SetTime(clk.Now().Add(time.Hour))
	claimedSent, err := transport.ClaimDueEgress(
		ctx, "worker-c", 10, time.Minute,
	)
	require.NoError(t, err)
	require.Empty(t, claimedSent)
}

func TestTransportStoreIngressTxRollbackAndCommit(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	transport, _ := newTransportStoreForTest(
		t,
		clock.NewTestClock(
			time.Unix(1_700_000_000, 0),
		),
	)

	localID := "client-local"
	remoteID := "server-remote"
	state := serverconn.AckState{
		PullCursor:          12,
		DispatchCommittedTo: 11,
		AckTarget:           11,
		AckCommittedTo:      10,
	}

	err := transport.RunInIngressTx(ctx, func(txCtx context.Context) error {
		require.NoError(
			t, transport.SaveIngressCursor(
				txCtx, localID, remoteID, state,
			),
		)

		return transportTestErr("rollback")
	})
	require.Error(t, err)

	loaded, err := transport.LoadIngressCursor(ctx, localID, remoteID)
	require.NoError(t, err)
	require.Equal(t, serverconn.AckState{}, loaded)

	require.NoError(
		t, transport.RunInIngressTx(
			ctx, func(txCtx context.Context) error {
				return transport.SaveIngressCursor(
					txCtx, localID, remoteID, state,
				)
			},
		),
	)

	loaded, err = transport.LoadIngressCursor(ctx, localID, remoteID)
	require.NoError(t, err)
	require.Equal(t, state, loaded)
}

type transportTestErr string

func (e transportTestErr) Error() string { return string(e) }
