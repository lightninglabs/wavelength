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
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}

	rawTimeout, err := cliStringFlag(cmd, "timeout")
	if err != nil {
		return context.WithCancel(parent)
	}

	timeout, err := time.ParseDuration(rawTimeout)
	if err != nil {
		return context.WithCancel(parent)
	}
	if timeout <= 0 {
		return context.WithCancel(parent)
	}

	return context.WithTimeout(parent, timeout)
}
