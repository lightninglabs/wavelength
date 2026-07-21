package waveclicommands

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRPCContextUsesDefaultTimeout verifies that daemon RPCs are bounded even
// when callers do not provide an explicit timeout.
func TestRPCContextUsesDefaultTimeout(t *testing.T) {
	t.Parallel()

	cmd := findRootCommand(t, newRootCmd(false), "getinfo")
	cmd.SetContext(t.Context())

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	require.WithinDuration(
		t, time.Now().Add(defaultRPCTimeout), deadline, time.Second,
	)
}

// TestRPCContextAllowsDisablingTimeout verifies that long-running callers can
// opt out while still retaining normal command cancellation.
func TestRPCContextAllowsDisablingTimeout(t *testing.T) {
	t.Parallel()

	root := newRootCmd(false)
	cmd := findRootCommand(t, root, "getinfo")
	cmd.SetContext(t.Context())
	require.NoError(t, root.PersistentFlags().Set("timeout", "0"))

	ctx, cancel := rpcContext(cmd)
	_, ok := ctx.Deadline()
	require.False(t, ok)

	cancel()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

// TestRoundsWatchBounds verifies the streaming command exposes deterministic
// duration and event-count completion controls for machine callers.
func TestRoundsWatchBounds(t *testing.T) {
	t.Parallel()

	cmd := newRoundsWatchCmd()

	maxEvents, err := cmd.Flags().GetUint32("max-events")
	require.NoError(t, err)
	require.Zero(t, maxEvents)

	watchFor, err := cmd.Flags().GetDuration("for")
	require.NoError(t, err)
	require.Zero(t, watchFor)
}
