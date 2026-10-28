package waved

import (
	"context"
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// TestServerConnectionUp pins the composition behind GetInfo's
// server_connected (issue #1467): the mailbox-ingress lifecycle signal AND,
// for a direct gRPC connection, a Ready transport. The flag previously
// reported the ingress signal alone, so it stayed true through a network
// outage even while every heartbeat failed.
func TestServerConnectionUp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		ingressUp     bool
		hasDirectConn bool
		connState     connectivity.State
		want          bool
	}{
		{
			name:      "ingress down reports down",
			ingressUp: false,
			want:      false,
		},
		{
			name:      "ingress down with ready conn reports down",
			ingressUp: false, hasDirectConn: true,
			connState: connectivity.Ready,
			want:      false,
		},
		{
			name:      "no direct conn follows ingress (REST)",
			ingressUp: true,
			want:      true,
		},
		{
			name:      "ready transport reports up",
			ingressUp: true, hasDirectConn: true,
			connState: connectivity.Ready,
			want:      true,
		},
		{
			name:      "idle transport reports down",
			ingressUp: true, hasDirectConn: true,
			connState: connectivity.Idle,
			want:      false,
		},
		{
			name:      "connecting transport reports down",
			ingressUp: true, hasDirectConn: true,
			connState: connectivity.Connecting,
			want:      false,
		},
		{
			name:      "transient failure reports down",
			ingressUp: true, hasDirectConn: true,
			connState: connectivity.TransientFailure,
			want:      false,
		},
		{
			name:      "shutdown transport reports down",
			ingressUp: true, hasDirectConn: true,
			connState: connectivity.Shutdown,
			want:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := serverConnectionUp(
				tc.ingressUp, tc.hasDirectConn, tc.connState,
			)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestServerConnectionStartup exercises status reads during asynchronous
// bootstrap. Under -race it catches reads of serverConn before the ingress
// flag publishes the connection.
func TestServerConnectionStartup(t *testing.T) {
	t.Parallel()

	conn, err := grpc.NewClient(
		"passthrough:///startup",
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})

	for range 100 {
		srv := &Server{}
		started := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			close(started)
			for !srv.serverConnected.Load() {
				_ = srv.isServerConnected()
				runtime.Gosched()
			}
		}()

		<-started
		srv.serverConn = conn
		srv.setServerConnected(true)
		<-done

		// Publication alone cannot report an idle transport as
		// connected.
		require.False(t, srv.isServerConnected())
	}
}

// TestServerConnectionTransport exercises the actual status method across
// transport loss and reconnection without changing the ingress flag.
func TestServerConnectionTransport(t *testing.T) {
	t.Parallel()

	listener := bufconn.Listen(1 << 20)
	operator := grpc.NewServer()
	go func() {
		_ = operator.Serve(listener)
	}()
	t.Cleanup(operator.Stop)

	// Replacing the endpoint simulates an operator restart while retaining
	// the same client connection, as in the daemon's reconnect path.
	var endpoint atomic.Pointer[bufconn.Listener]
	endpoint.Store(listener)
	conn, err := grpc.NewClient(
		"passthrough:///operator",
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (
			net.Conn, error) {

			return endpoint.Load().DialContext(ctx)
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})

	srv := &Server{serverConn: conn}
	require.False(t, srv.isServerConnected())
	srv.setServerConnected(true)
	require.False(t, srv.isServerConnected())

	conn.Connect()
	require.Eventually(
		t, srv.isServerConnected, 5*time.Second, 10*time.Millisecond,
	)

	operator.Stop()
	require.Eventually(t, func() bool {
		return !srv.isServerConnected()
	}, 5*time.Second, 10*time.Millisecond)

	replacement := bufconn.Listen(1 << 20)
	restarted := grpc.NewServer()
	go func() {
		_ = restarted.Serve(replacement)
	}()
	t.Cleanup(restarted.Stop)
	endpoint.Store(replacement)
	conn.ResetConnectBackoff()
	conn.Connect()
	require.Eventually(
		t, srv.isServerConnected, 5*time.Second, 10*time.Millisecond,
	)

	// Even a Ready connection cannot override shutdown or incompatibility.
	srv.setServerConnected(false)
	require.False(t, srv.isServerConnected())
}
