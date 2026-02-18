package mailboxclient_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/arkrpc"
	mailboxclient "github.com/lightninglabs/darepo-client/mailbox/client"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

// testArkServer implements a tiny mailbox RPC server used by unit tests.
type testArkServer struct {
	resp *arkrpc.GetInfoResponse
}

// GetInfo returns a fixed response for tests.
func (s *testArkServer) GetInfo(ctx context.Context,
	req *arkrpc.GetInfoRequest) (*arkrpc.GetInfoResponse, error) {

	_ = ctx
	_ = req

	return s.resp, nil
}

// runOperator polls operatorMailboxID and responds to requests using mux.
func runOperator(ctx context.Context, edge mailboxpb.MailboxServiceClient,
	operatorMailboxID string, mux *mailboxrpc.ServeMux) error {

	var cursor uint64

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		pull, err := edge.Pull(ctx, &mailboxpb.PullRequest{
			MailboxId:     operatorMailboxID,
			MaxEnvelopes:  10,
			WaitTimeoutMs: 50,
			Cursor:        cursor,
		})
		if err != nil {
			return err
		}
		if pull.Status == nil || !pull.Status.Ok {
			return fmt.Errorf("operator pull failed")
		}

		for _, env := range pull.Envelopes {
			err := handleOperatorEnvelope(
				ctx, edge, operatorMailboxID, mux, env,
			)
			if err != nil {
				return err
			}
		}

		if pull.NextCursor > cursor {
			ack, err := edge.AckUpTo(ctx, &mailboxpb.AckUpToRequest{
				MailboxId: operatorMailboxID,
				Cursor:    pull.NextCursor,
			})
			if err != nil {
				return err
			}
			if ack.Status == nil || !ack.Status.Ok {
				return fmt.Errorf("operator ack failed")
			}

			cursor = pull.NextCursor
		}
	}
}

// handleOperatorEnvelope serves a request envelope and sends a response.
func handleOperatorEnvelope(ctx context.Context,
	edge mailboxpb.MailboxServiceClient, operatorMailboxID string,
	mux *mailboxrpc.ServeMux, env *mailboxpb.Envelope) error {

	if env == nil || env.Rpc == nil {
		return nil
	}
	if env.Rpc.Kind != mailboxpb.RpcMeta_KIND_REQUEST {
		return nil
	}

	if env.Body == nil {
		return fmt.Errorf("missing request body")
	}

	resp, err := mux.ServeRPC(ctx, env.Rpc.Service, env.Rpc.Method,
		env.Body.Value)
	if err != nil {
		return err
	}

	respAny, err := anypb.New(resp)
	if err != nil {
		return err
	}

	replyTo := env.Rpc.ReplyTo
	if replyTo == "" {
		return fmt.Errorf("missing reply_to")
	}

	responseEnv := &mailboxpb.Envelope{
		ProtocolVersion: env.ProtocolVersion,
		MsgId:           "resp-" + env.MsgId,
		Sender:          operatorMailboxID,
		Recipient:       replyTo,
		CreatedAtUnixMs: time.Now().UnixMilli(),
		Body:            respAny,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
			Service:       env.Rpc.Service,
			Method:        env.Rpc.Method,
			CorrelationId: env.Rpc.CorrelationId,
		},
	}

	send, err := edge.Send(ctx, &mailboxpb.SendRequest{
		Envelope: responseEnv,
	})
	if err != nil {
		return err
	}
	if send.Status == nil || !send.Status.Ok {
		return fmt.Errorf("operator send failed")
	}

	return nil
}

// TestClient_GetInfoRoundTrip verifies a basic request/response round trip.
func TestClient_GetInfoRoundTrip(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	edge := &fakeMailboxServiceClient{mb: mb}

	mux := mailboxrpc.NewServeMux()
	arkrpc.RegisterArkServiceMailboxServer(mux, &testArkServer{
		resp: &arkrpc.GetInfoResponse{
			Version:     "v-test",
			Network:     "regtest",
			BlockHeight: 123,
		},
	})

	operatorCtx, cancel := context.WithCancel(t.Context())
	defer cancel()

	operatorErr := make(chan error, 1)
	go func() {
		operatorErr <- runOperator(operatorCtx, edge, "operator", mux)
	}()

	cfg := mailboxclient.DefaultConfig()
	cfg.Edge = edge
	cfg.LocalMailboxID = "client-1"
	cfg.RemoteMailboxID = "operator"
	cfg.ProtocolVersion = 1
	cfg.PullWaitTimeout = 20 * time.Millisecond

	rpc, err := mailboxclient.New(cfg)
	require.NoError(t, err)
	defer rpc.Stop()

	client := arkrpc.NewArkServiceMailboxClient(rpc)

	resp, err := client.GetInfo(t.Context(), &arkrpc.GetInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "v-test", resp.Version)
	require.Equal(t, "regtest", resp.Network)
	require.Equal(t, uint32(123), resp.BlockHeight)

	cancel()
	require.NoError(t, <-operatorErr)
}

// TestClient_ConcurrentInFlightDoesNotDrop verifies that cursor-based acking
// does not discard a response for a different in-flight call.
func TestClient_ConcurrentInFlightDoesNotDrop(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	edge := &fakeMailboxServiceClient{mb: mb}

	mux := mailboxrpc.NewServeMux()
	arkrpc.RegisterArkServiceMailboxServer(mux, &testArkServer{
		resp: &arkrpc.GetInfoResponse{
			Version:     "v-test",
			Network:     "regtest",
			BlockHeight: 999,
		},
	})

	operatorCtx, cancel := context.WithCancel(t.Context())
	defer cancel()

	operatorErr := make(chan error, 1)
	go func() {
		operatorErr <- runOperator(operatorCtx, edge, "operator", mux)
	}()

	cfg := mailboxclient.DefaultConfig()
	cfg.Edge = edge
	cfg.LocalMailboxID = "client-1"
	cfg.RemoteMailboxID = "operator"
	cfg.ProtocolVersion = 1
	cfg.PullWaitTimeout = 20 * time.Millisecond

	rpc, err := mailboxclient.New(cfg)
	require.NoError(t, err)
	defer rpc.Stop()

	type result struct {
		resp *arkrpc.GetInfoResponse
		err  error
	}

	call := func(ctx context.Context, correlationID string) result {
		var out result

		result, err := rpc.SendRPC(
			ctx,
			mailboxrpc.ServiceMethod{
				Service: "arkrpc.ArkService",
				Method:  "GetInfo",
			},
			&arkrpc.GetInfoRequest{},
			mailboxrpc.RPCOptions{
				CorrelationID:  correlationID,
				IdempotencyKey: correlationID,
			},
		)
		if err != nil {
			out.err = err
			return out
		}

		resp := new(arkrpc.GetInfoResponse)
		err = rpc.AwaitRPC(ctx, result.CorrelationID, resp)
		out.err = err
		out.resp = resp

		return out
	}

	ctx, cancelCalls := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelCalls()

	ch1 := make(chan result, 1)
	ch2 := make(chan result, 1)
	go func() { ch1 <- call(ctx, "corr-1") }()
	go func() { ch2 <- call(ctx, "corr-2") }()

	r1 := <-ch1
	r2 := <-ch2

	require.NoError(t, r1.err)
	require.NoError(t, r2.err)
	require.Equal(t, uint32(999), r1.resp.BlockHeight)
	require.Equal(t, uint32(999), r2.resp.BlockHeight)

	cancel()
	require.NoError(t, <-operatorErr)
}
