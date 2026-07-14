package waveclicommands

import (
	"fmt"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

// parseRequest populates the given proto request from either the
// --json flag (raw JSON payload, agent-first path) or from bespoke
// CLI flags via the fromFlags callback (human-first path). When
// --json is provided, fromFlags is never called.
//
// This gives agents a zero-translation-loss input path: they can
// pass the full RPC request payload as JSON, mapping directly to
// the proto schema, without going through layers of flag
// abstractions.
func parseRequest[T proto.Message](cmd *cobra.Command, req T,
	fromFlags func() error) error {

	raw, _ := cmd.Flags().GetString("json")
	if raw != "" {
		if err := jsonUnmarshalOpts.Unmarshal(
			[]byte(raw), req,
		); err != nil {
			return fmt.Errorf("invalid --json payload: %w", err)
		}

		return nil
	}

	// No JSON payload — fall through to bespoke flags.
	if fromFlags != nil {
		return fromFlags()
	}

	return nil
}
