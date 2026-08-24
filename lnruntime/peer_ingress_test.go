package lnruntime

import (
	"context"
	"database/sql"
	"fmt"
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

// TestPeerMessageIngressParksOrderedLane proves a temporarily unavailable lnd
// endpoint cannot dead-letter one message and advance to later BOLT traffic.
func TestPeerMessageIngressParksOrderedLane(t *testing.T) {
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

	const failuresBeforeReady = 7
	attempts := 0
	handled := make(chan uint16, 2)
	ingress, err := newPeerMessageIngress(
		PeerMessageIngressConfig{
			ActorID: "test-peer-ingress-park",
			Store:   store,
			Handler: func(_ context.Context,
				message lnwire.Message) error {

				ping, ok := message.(*lnwire.Ping)
				if !ok {
					return nil
				}
				attempts++
				if attempts <= failuresBeforeReady {
					return fmt.Errorf("endpoint " +
						"unavailable")
				}
				handled <- ping.NumPongBytes

				return nil
			},
		},
		func(error, int) (bool, time.Duration) {
			return true, time.Millisecond
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()

		require.NoError(t, ingress.StopAndWait(ctx))
	})

	dispatch := ingress.Dispatcher()
	for _, pongBytes := range []uint16{11, 12} {
		payload, err := MarshalPeerMessage(lnwire.NewPing(pongBytes))
		require.NoError(t, err)
		body, err := anypb.New(&wrapperspb.BytesValue{Value: payload})
		require.NoError(t, err)
		err = store.ExecTx(
			t.Context(), false,
			func(txCtx context.Context,
				_ actor.DeliveryStore) error {

				return dispatch(txCtx, &mailboxpb.Envelope{
					Body: body,
				})
			},
		)
		require.NoError(t, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for _, expected := range []uint16{11, 12} {
		select {
		case actual := <-handled:
			require.Equal(t, expected, actual)

		case <-ctx.Done():
			t.Fatal("parked peer message did not resume")
		}
	}
	require.Equal(t, failuresBeforeReady+2, attempts)
	deadLetters, err := store.ListDeadLetters(
		t.Context(),
		"test-peer-ingress-park", 10,
	)
	require.NoError(t, err)
	require.Empty(t, deadLetters)
}

// TestPeerMessageRetryPolicyNeverAdvances verifies the production policy
// always parks, including after attempt counts that previously dead-lettered.
func TestPeerMessageRetryPolicyNeverAdvances(t *testing.T) {
	t.Parallel()

	for _, attempts := range []int{0, 5, 100, 1_000_000} {
		retry, delay := parkPeerMessageRetryPolicy(
			fmt.Errorf("endpoint unavailable"), attempts,
		)
		require.True(t, retry)
		require.Positive(t, delay)
		require.LessOrEqual(t, delay, peerMessageMaxRetryDelay)
	}
}
