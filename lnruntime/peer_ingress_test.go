package lnruntime

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/actordelivery"
	adsqlc "github.com/lightninglabs/wavelength/db/actordelivery/sqlc"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestPeerMessageIngressDefersLNDUntilCommit proves mailbox ingress only
// stages BOLT traffic in the outer transaction. Native lnd may synchronously
// produce a durable reply, so its handler must not run while that transaction
// still owns SQLite's writer lock.
func TestPeerMessageIngressDefersLNDUntilCommit(t *testing.T) {
	t.Parallel()

	rawStore := db.NewTestDB(t)
	actorQueries := adsqlc.New(rawStore.DB)
	actorDB := db.NewTransactionExecutor(
		rawStore.BaseDB,
		func(tx *sql.Tx) actordelivery.ActorDeliveryQueries {
			return actorQueries.WithTx(tx)
		},
		btclog.Disabled,
	)
	store := actordelivery.NewTxAwareActorDeliveryStore(
		actorDB, rawStore.BaseDB, clock.NewDefaultClock(),
	)

	handled := make(chan *lnwire.Ping, 1)
	ingress, err := NewPeerMessageIngress(PeerMessageIngressConfig{
		ActorID: "test-peer-ingress",
		Store:   store,
		Handler: func(_ context.Context, message lnwire.Message) error {
			ping, ok := message.(*lnwire.Ping)
			if !ok {
				return nil
			}
			handled <- ping

			return nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()

		require.NoError(t, ingress.StopAndWait(ctx))
	})

	payload, err := MarshalPeerMessage(lnwire.NewPing(11))
	require.NoError(t, err)
	body, err := anypb.New(&wrapperspb.BytesValue{Value: payload})
	require.NoError(t, err)
	envelope := &mailboxpb.Envelope{Body: body}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	staged := make(chan struct{})
	releaseCommit := make(chan struct{})
	dispatchResult := make(chan error, 1)
	dispatch := ingress.Dispatcher()
	go func() {
		dispatchResult <- store.ExecTx(
			ctx, false, func(txCtx context.Context,
				_ actor.DeliveryStore) error {

				if err := dispatch(
					txCtx, envelope,
				); err != nil {
					return err
				}
				close(staged)

				select {
				case <-releaseCommit:
					return nil

				case <-txCtx.Done():
					return txCtx.Err()
				}
			},
		)
	}()

	select {
	case <-staged:
	case <-ctx.Done():
		t.Fatal("peer message was not staged")
	}

	select {
	case <-handled:
		t.Fatal("lnd handler ran before ingress transaction committed")

	case <-time.After(100 * time.Millisecond):
	}

	close(releaseCommit)
	select {
	case err := <-dispatchResult:
		require.NoError(t, err)

	case <-ctx.Done():
		t.Fatal("ingress transaction did not commit")
	}

	select {
	case ping := <-handled:
		require.EqualValues(t, 11, ping.NumPongBytes)

	case <-ctx.Done():
		t.Fatal("committed peer message was not handled")
	}
}
