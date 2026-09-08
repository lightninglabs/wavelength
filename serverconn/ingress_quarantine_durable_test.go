package serverconn_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	adsqlc "github.com/lightninglabs/wavelength/db/actordelivery/sqlc"
	"github.com/lightninglabs/wavelength/internal/actortest"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// quarantineTestRouter builds a real durable recipient without starting its
// consumer, so tests can inspect durable handoff rows after ingress stops.
func quarantineTestRouter(t *testing.T, store actor.DeliveryStore, lane string,
	repaired bool, inMemory ...bool) serverconn.DispatcherMap {

	t.Helper()
	system := actor.NewActorSystem()
	t.Cleanup(
		func() {
			require.NoError(
				t,
				system.Shutdown(
					context.Background(),
				),
			)
		},
	)
	codec := actor.NewMessageCodec()
	codec.MustRegister(
		actortest.IncrementMsgType,
		func() actor.TLVMessage { return &actortest.IncrementMsg{} },
	)
	behavior := actor.NewFunctionBehavior(
		func(_ context.Context,
			_ *actortest.IncrementMsg) fn.Result[int] {

			return fn.Ok(0)
		},
	)
	var ref actor.ActorRef[*actortest.IncrementMsg, int]
	if len(inMemory) > 0 && inMemory[0] {
		target := actor.NewActor(
			actor.ActorConfig[*actortest.IncrementMsg, int]{
				ID:          lane,
				Behavior:    behavior,
				MailboxSize: 4,
			},
		)
		t.Cleanup(target.Stop)
		ref = target.Ref()
	} else {
		target := actor.NewDurableActor(actor.DefaultDurableActorConfig[
			*actortest.IncrementMsg,
			int,
		](
			lane, behavior, store, codec,
		)).UnwrapOrFail(t)
		t.Cleanup(target.Stop)
		ref = target.Ref()
	}
	key := actor.NewServiceKey[*actortest.IncrementMsg, int](lane)
	require.NoError(
		t,
		actor.RegisterWithReceptionist(
			system.Receptionist(), key, ref,
		),
	)
	router := serverconn.NewEventRouter(system)
	serverconn.AddRoute(
		router, serverconn.EventRouteConfig[
			*actortest.IncrementMsg,
			int,
		]{
			Service: "test",
			Method:  "Event",
			Key:     key,
			NewEvent: func() proto.Message {
				return &wrapperspb.Int64Value{}
			},
			Adapt: func(p proto.Message) (*actortest.IncrementMsg,
				error) {

				event, ok := p.(*wrapperspb.Int64Value)
				if !ok {
					return nil, errors.New("unexpected " +
						"event type")
				}
				if event.Value < 0 && !repaired {
					return nil, errors.New("unsupported " +
						"event value")
				}

				return &actortest.IncrementMsg{
					Amount: event.Value,
				}, nil
			},
		},
	)

	return router.AsDispatcherMap()
}

// TestIngressPoisonQuarantineAndRecovery proves parseable poison cannot wedge
// later events, and exact raw evidence survives ACK/reopen until a repaired
// adapter commits the missing durable consumer handoff.
func TestIngressPoisonQuarantineAndRecovery(t *testing.T) {
	ctx := context.Background()
	dsn := ingressTestDSN(t)
	raw, store := openIngressStore(t, dsn)
	lane := t.Name() + uuid.NewString()
	body, err := anypb.New(wrapperspb.Int64(-1))
	require.NoError(t, err)
	bad := &mailboxpb.Envelope{
		ProtocolVersion:    1,
		ArkProtocolVersion: 1,
		EventSeq:           1,
		MsgId:              "delivery-v1:" + uuid.NewString(),
		Body:               body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_EVENT,
			Service: "test",
			Method:  "Event",
		},
	}
	good, ok := proto.Clone(bad).(*mailboxpb.Envelope)
	require.True(t, ok)
	good.EventSeq = 3
	good.MsgId = "delivery-v1:" + uuid.NewString()
	good.Body, err = anypb.New(wrapperspb.Int64(7))
	require.NoError(t, err)
	done := make(chan struct{})
	edge := &durableIngressEdge{}
	edge.pull = func(ctx context.Context, req *mailboxpb.PullRequest) (
		*mailboxpb.PullResponse, error) {

		if req.Cursor > 3 {
			<-ctx.Done()

			return nil, ctx.Err()
		}

		return &mailboxpb.PullResponse{
			Status: &mailboxpb.Status{
				Ok: true,
			},
			Envelopes: []*mailboxpb.Envelope{
				bad,
				good,
			},
			NextCursor: 4,
		}, nil
	}
	edge.ack = func(_ context.Context, req *mailboxpb.AckUpToRequest) (
		*mailboxpb.AckUpToResponse, error) {

		if req.Cursor == 4 {
			close(done)
		}

		return &mailboxpb.AckUpToResponse{
			Status: &mailboxpb.Status{
				Ok: true,
			},
		}, nil
	}
	cfg := serverconn.DefaultConnectorConfig()
	cfg.ArkProtocolVersion = 1
	cfg.LocalMailboxID = lane
	cfg.RemoteMailboxID = "server"
	cfg.Store = store
	cfg.Edge = edge
	cfg.Dispatchers = quarantineTestRouter(t, store, lane, false)
	runDurableIngress(t, cfg, done)
	row, err := store.PeekNextMessage(ctx, lane)
	require.NoError(t, err)
	require.NotNil(
		t, row, "healthy event after poison must reach durable inbox",
	)
	_, err = store.AckMessageByID(ctx, row.ID)
	require.NoError(t, err)
	// Read raw evidence through the generated query, independent of the
	// loop.
	q := adsqlc.New(raw)
	var state serverconn.AckState
	checkpoint, err := store.LoadCheckpoint(
		ctx, serverconn.DurableActorID(lane),
	)
	require.NoError(t, err)
	require.NoError(t, state.Decode(bytes.NewReader(checkpoint.StateData)))
	require.Equal(t, uint64(4), state.PullCursor)
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(bad)
	require.NoError(t, err)
	// Compute only the configured lane key; the payload ID is discovered in
	// SQL.
	scope := fmt.Appendf(nil, "%d:%s%s", len(lane), lane, "server")
	digest := sha256.Sum256(scope)
	evidenceLane := hex.EncodeToString(digest[:])
	entries, err := q.ListIngressQuarantine(ctx, evidenceLane)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, encoded, entries[0].Envelope)
	require.Contains(t, entries[0].Reason, "unsupported event value")
	require.NoError(t, raw.Close())
	raw, store = openIngressStore(t, dsn)
	cfg.Store = store
	cfg.Dispatchers = quarantineTestRouter(t, store, lane, true)
	done = make(chan struct{})
	edge.pull = func(ctx context.Context, _ *mailboxpb.PullRequest) (
		*mailboxpb.PullResponse, error) {

		close(done)
		<-ctx.Done()

		return nil, ctx.Err()
	}
	edge.ack = func(_ context.Context, _ *mailboxpb.AckUpToRequest) (
		*mailboxpb.AckUpToResponse, error) {

		return &mailboxpb.AckUpToResponse{
			Status: &mailboxpb.Status{
				Ok: true,
			},
		}, nil
	}
	// A previous expired receipt may still exist before bounded cleanup. It
	// cannot prove the current recovered payload reached a durable inbox.
	identity, err := json.Marshal([]string{
		"ingress-event/v1", lane, "server", bad.Sender,
		bad.Rpc.Service, bad.Rpc.Method, bad.MsgId,
	})
	require.NoError(t, err)
	oldID := sha256.Sum256(identity)
	q = adsqlc.New(raw)
	_, err = q.AdmitIngressReceipt(ctx, adsqlc.AdmitIngressReceiptParams{
		ID: hex.EncodeToString(oldID[:]), PayloadHash: []byte(
			"old-payload",
		),
		MailboxID: lane, ConsumedAt: 1, ExpiresAt: 2,
	})
	require.NoError(t, err)
	// A corrected adapter can reach an in-memory target, but that is
	// insufficient to retire crash-recovery evidence.
	cfg.Dispatchers = quarantineTestRouter(t, store, lane, true, true)
	runDurableIngress(t, cfg, done)
	evidence, ok := store.(mailboxconn.IngressQuarantineStore)
	require.True(t, ok)
	pending, err := evidence.ListIngressQuarantine(ctx, evidenceLane)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, int64(1), pending[0].Attempts)
	require.Equal(t, encoded, pending[0].Envelope)
	row, err = store.PeekNextMessage(ctx, lane)
	require.NoError(t, err)
	require.Nil(t, row)

	done = make(chan struct{})
	cfg.Dispatchers = quarantineTestRouter(t, store, lane, true)
	runDurableIngress(t, cfg, done)
	evidence, ok = store.(mailboxconn.IngressQuarantineStore)
	require.True(t, ok)
	remaining, err := evidence.ListIngressQuarantine(ctx, evidenceLane)
	require.NoError(t, err)
	require.Empty(
		t, remaining, "only durable recovery may remove raw evidence",
	)
	row, err = store.PeekNextMessage(ctx, lane)
	require.NoError(t, err)
	require.NotNil(t, row)
	msg, err := codecForQuarantineTest().Decode(row.Payload)
	require.NoError(t, err)
	recovered, ok := msg.(*actortest.IncrementMsg)
	require.True(t, ok)
	require.Equal(t, int64(-1), recovered.Amount)
}

// codecForQuarantineTest decodes the retained consumer payload for SQL
// evidence.
func codecForQuarantineTest() *actor.MessageCodec {
	codec := actor.NewMessageCodec()
	codec.MustRegister(
		actortest.IncrementMsgType,
		func() actor.TLVMessage { return &actortest.IncrementMsg{} },
	)

	return codec
}
