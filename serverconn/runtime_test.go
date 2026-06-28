package serverconn

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDurableActorID verifies the stable actor ID derivation helper.
func TestDurableActorID(t *testing.T) {
	t.Parallel()

	require.Equal(t, "serverconn-client-1", DurableActorID("client-1"))
}

// TestNewRuntime_ValidateConfig verifies runtime construction rejects missing
// required configuration.
func TestNewRuntime_ValidateConfig(t *testing.T) {
	t.Parallel()

	_, err := NewRuntime(ConnectorConfig{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "connector store is required")

	_, err = NewRuntime(ConnectorConfig{
		Store: newMemCheckpointStore(),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "connector edge is required")

	mb := newInMemoryMailbox()
	_, err = NewRuntime(ConnectorConfig{
		Store: newMemCheckpointStore(),
		Edge:  &fakeMailboxServiceClient{mb: mb},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "local mailbox id is required")

	_, err = NewRuntime(ConnectorConfig{
		Store:          newMemCheckpointStore(),
		Edge:           &fakeMailboxServiceClient{mb: mb},
		LocalMailboxID: "client-1",
	})
	require.Error(t, err)
	require.Contains(
		t, err.Error(), "remote mailbox id is required",
	)
}

// TestNewRuntimeArkVersionBinding verifies runtime construction rejects a zero
// Ark protocol version and accepts an explicit v1 as well as a synthetic v2.
// The synthetic v2 case proves selection and binding work without adding v2 to
// any production default.
func TestNewRuntimeArkVersionBinding(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()

	baseCfg := func() ConnectorConfig {
		cfg := DefaultConnectorConfig()
		cfg.Store = newMemCheckpointStore()
		cfg.Edge = &fakeMailboxServiceClient{mb: mb}
		cfg.LocalMailboxID = "client-1"
		cfg.RemoteMailboxID = "server-1"

		return cfg
	}

	// A zero Ark protocol version is rejected: a runtime must always carry
	// an explicit bound version.
	zeroCfg := baseCfg()
	zeroCfg.ArkProtocolVersion = 0
	_, err := NewRuntime(zeroCfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ark protocol version")

	// An explicit Ark v1 is accepted, and the default mailbox transport
	// version is bound.
	v1Cfg := baseCfg()
	v1Cfg.ArkProtocolVersion = 1
	rt, err := NewRuntime(v1Cfg)
	require.NoError(t, err)
	require.NotNil(t, rt)
	require.Equal(
		t, uint32(1), rt.Connector().cfg.ArkProtocolVersion,
	)
	require.NotZero(t, rt.Connector().cfg.MailboxProtocolVersion)

	// A synthetic Ark v2 is accepted too, proving binding is not pinned to
	// v1 even though no production default advertises v2.
	v2Cfg := baseCfg()
	v2Cfg.ArkProtocolVersion = 2
	rt2, err := NewRuntime(v2Cfg)
	require.NoError(t, err)
	require.Equal(
		t, uint32(2), rt2.Connector().cfg.ArkProtocolVersion,
	)
}

// TestNewRuntime_DefaultCodec verifies runtime construction fills a default
// codec when one is not supplied.
func TestNewRuntime_DefaultCodec(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	cfg := newTestConnectorConfig(mb, newMemCheckpointStore())
	cfg.Codec = nil

	runtime, err := NewRuntime(cfg)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	require.NotNil(t, runtime.Unary())
	require.NotNil(t, runtime.Connector())
	require.NotNil(t, runtime.TellRef())
	require.NotNil(t, runtime.Ref())
	require.Equal(
		t, DurableActorID("client-1"), runtime.Ref().ID(),
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
