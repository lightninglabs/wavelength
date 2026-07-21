package waveclicommands

import (
	"context"
	"time"

	"github.com/spf13/cobra"
)

// rpcContext returns a child of the command context bounded by the global
// RPC timeout. A zero timeout disables the deadline while preserving signal
// and caller cancellation.
func rpcContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return rpcContextFrom(cmd, commandContext(cmd))
}

// rpcContextFrom returns a child of parent bounded by the global RPC timeout.
// It lets multi-step commands apply the per-RPC bound inside their separate
// overall operation deadline.
func rpcContextFrom(cmd *cobra.Command,
	parent context.Context) (context.Context, context.CancelFunc) {

	rawTimeout, err := cliStringFlag(cmd, "timeout")
	if err != nil {
		return context.WithCancel(parent)
	}

	timeout, err := time.ParseDuration(rawTimeout)
	if err != nil || timeout <= 0 {
		return context.WithCancel(parent)
	}

	return context.WithTimeout(parent, timeout)
}

// commandContext returns the command's caller-owned context or a background
// context for direct command invocations that did not install one.
func commandContext(cmd *cobra.Command) context.Context {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}

	return parent
}
