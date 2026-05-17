package serverconn

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRuntimeID verifies the stable runtime ID derivation helper.
func TestRuntimeID(t *testing.T) {
	t.Parallel()

	require.Equal(t, "serverconn-client-1", RuntimeID("client-1"))
}

// TestNewRuntime_ValidateConfig verifies runtime construction rejects missing
// required configuration.
func TestNewRuntime_ValidateConfig(t *testing.T) {
	t.Parallel()

	_, err := NewRuntime(ConnectorConfig{})
	require.Error(t, err)
	require.Contains(
		t, err.Error(),
		"connector transport store is required",
	)

	_, err = NewRuntime(ConnectorConfig{
		Transport: newMemTransportStore(),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "connector edge is required")

	mb := newInMemoryMailbox()
	_, err = NewRuntime(ConnectorConfig{
		Transport: newMemTransportStore(),
		Edge:      &fakeMailboxServiceClient{mb: mb},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "local mailbox id is required")

	_, err = NewRuntime(ConnectorConfig{
		Transport:      newMemTransportStore(),
		Edge:           &fakeMailboxServiceClient{mb: mb},
		LocalMailboxID: "client-1",
	})
	require.Error(t, err)
	require.Contains(
		t, err.Error(), "remote mailbox id is required",
	)
}

// TestNewRuntime_ConstructsRefs verifies runtime construction wires the
// in-memory actor and SQL-backed transport references.
func TestNewRuntime_ConstructsRefs(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	cfg := newTestConnectorConfig(mb, newMemCheckpointStore())

	runtime, err := NewRuntime(cfg)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	require.NotNil(t, runtime.Unary())
	require.NotNil(t, runtime.Connector())
	require.NotNil(t, runtime.TellRef())
	require.NotNil(t, runtime.Ref())
	require.Equal(
		t, RuntimeID("client-1"), runtime.Ref().ID(),
	)
}

// TestRuntime_StartStop verifies runtime lifecycle methods run and return
// promptly when the parent context is cancelled.
func TestRuntime_StartStop(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	cfg := newTestConnectorConfig(mb, newMemCheckpointStore())
	cfg.PullWaitTimeout = 25 * time.Millisecond

	runtime, err := NewRuntime(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	require.NoError(t, runtime.Start(ctx))

	time.Sleep(50 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() {
		runtime.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime Stop did not return")
	}
}
