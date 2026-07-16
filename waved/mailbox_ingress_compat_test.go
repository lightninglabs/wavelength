package waved

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// permanentPullEdge is a mailbox edge whose Pull returns a permanent version
// status, so an ingress loop transitions the connector to its incompatible
// state on its first poll. Send and AckUpTo succeed.
type permanentPullEdge struct{}

func (permanentPullEdge) Send(_ context.Context, _ *mailboxpb.SendRequest,
	_ ...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	return &mailboxpb.SendResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
	}, nil
}

func (permanentPullEdge) Pull(_ context.Context, _ *mailboxpb.PullRequest,
	_ ...grpc.CallOption) (*mailboxpb.PullResponse, error) {

	return &mailboxpb.PullResponse{
		Status: &mailboxpb.Status{
			Ok:   false,
			Code: mailboxconn.StatusUpgradeRequired,
		},
	}, nil
}

func (permanentPullEdge) AckUpTo(_ context.Context, _ *mailboxpb.AckUpToRequest,
	_ ...grpc.CallOption) (*mailboxpb.AckUpToResponse, error) {

	return &mailboxpb.AckUpToResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
	}, nil
}

// okPullEdge is a mailbox edge that succeeds on every operation, returning
// empty long-poll results. Used when the runtime is pre-marked incompatible so
// the edge behavior is irrelevant.
type okPullEdge struct{}

func (okPullEdge) Send(_ context.Context, _ *mailboxpb.SendRequest,
	_ ...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	return &mailboxpb.SendResponse{Status: &mailboxpb.Status{Ok: true}}, nil
}

func (okPullEdge) Pull(_ context.Context, _ *mailboxpb.PullRequest,
	_ ...grpc.CallOption) (*mailboxpb.PullResponse, error) {

	return &mailboxpb.PullResponse{Status: &mailboxpb.Status{Ok: true}}, nil
}

func (okPullEdge) AckUpTo(_ context.Context, _ *mailboxpb.AckUpToRequest,
	_ ...grpc.CallOption) (*mailboxpb.AckUpToResponse, error) {

	return &mailboxpb.AckUpToResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
	}, nil
}

// newCompatTestServer wires a Server with a real serverconn.Runtime built from
// the given edge and a DB-backed delivery store, with the incompatibility
// callback routed to onServerIncompatible (which clears server_connected).
func newCompatTestServer(t *testing.T,
	edge mailboxpb.MailboxServiceClient) *Server {

	t.Helper()

	_, deliveryStore, _ := newSendOORTestStores(t)

	s := &Server{log: btclog.Disabled}

	cfg := serverconn.DefaultConnectorConfig()
	cfg.Edge = edge
	cfg.Store = deliveryStore
	cfg.LocalMailboxID = "client-1"
	cfg.RemoteMailboxID = "server-1"
	cfg.ArkProtocolVersion = 1
	cfg.OnIncompatible = s.onServerIncompatible

	rt, err := serverconn.NewRuntime(cfg)
	require.NoError(t, err)

	s.runtime = rt

	return s
}

// TestStartMailboxIngressConcurrentIncompatible proves that an incompatibility
// landing while ingress starts (here, the ingress loop's first Pull returns a
// permanent version status) always wins over the startup "connected" write:
// server_connected ends false. This is the concurrent-start race the fix
// closes by setting connected=true before StartIngress so any callback's false
// is written afterward.
func TestStartMailboxIngressConcurrentIncompatible(t *testing.T) {
	t.Parallel()

	s := newCompatTestServer(t, permanentPullEdge{})

	s.runtime.StartEgress()

	// StopAndWait (not the fire-and-forget Stop) so the durable egress
	// actor's goroutine has fully exited before the t.Cleanup chain closes
	// and removes the shared SQLite DB. Stop only cancels the actor context
	// and returns immediately, leaving the egress loop free to keep issuing
	// queries against the handle while cleanup runs; an in-flight
	// connection re-materializes the WAL/-shm sidecar files, so the tempdir
	// RemoveAll then fails with "directory not empty".
	defer func() {
		require.NoError(t, s.runtime.StopAndWait(t.Context()))
	}()

	// StartIngress itself succeeds; the ingress goroutine then transitions.
	require.NoError(t, s.startMailboxIngress(t.Context()))

	require.Eventually(t, func() bool {
		return !s.isServerConnected()
	}, 5*time.Second, 10*time.Millisecond)
}

// TestStartMailboxIngressAlreadyIncompatible proves that when the runtime is
// already incompatible, startMailboxIngress returns an error and leaves
// server_connected false rather than reporting a healthy connection.
func TestStartMailboxIngressAlreadyIncompatible(t *testing.T) {
	t.Parallel()

	s := newCompatTestServer(t, okPullEdge{})

	s.runtime.StartEgress()

	// Drain the durable egress actor before the shared SQLite DB is torn
	// down by t.Cleanup; see the note in the concurrent-incompatible test.
	defer func() {
		require.NoError(t, s.runtime.StopAndWait(t.Context()))
	}()

	// Mark incompatible up front; the callback clears server_connected.
	s.runtime.MarkIncompatible(
		t.Context(),
		mailboxconn.NewStatusError(
			"refresh", &mailboxpb.Status{
				Ok:   false,
				Code: mailboxconn.StatusArkVersionMismatch,
			},
		),
	)

	err := s.startMailboxIngress(t.Context())
	require.Error(t, err)
	require.False(t, s.isServerConnected())
}
