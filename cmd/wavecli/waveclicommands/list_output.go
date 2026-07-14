package waveclicommands

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

// addListOutputFlags registers the standard --fields / --ndjson flags
// that every list-shaped ark.* command needs to give agents
// context-window discipline over potentially-large responses. Inline
// duplication across cmd_vtxos.go, cmd_rounds.go, cmd_oor.go,
// cmd_sweep.go, and cmd_ark.go would let one of them silently drift
// past the documented behavior.
func addListOutputFlags(cmd *cobra.Command, itemName string) {
	cmd.Flags().String("fields", "",
		"comma-separated field names to include (response is "+
			"filtered before printing)")
	cmd.Flags().Bool("ndjson", false,
		fmt.Sprintf("emit one JSON object per %s (newline-delimited); "+
			"pairs with --fields to shrink each line further",
			itemName))
}

// renderListOutput emits resp as proto-JSON, applying the --fields
// field mask and the --ndjson streaming flag the agent set. items is
// the repeated payload on resp (one proto.Message per row); pass nil
// to disable --ndjson on responses that don't carry a list shape.
// The behavior mirrors what vtxos list shipped first:
//
//   - --ndjson alone: print one JSON object per item on its own line.
//   - --ndjson with --fields: print each item filtered to the named
//     fields.
//   - --fields alone: print the whole response filtered to the named
//     top-level keys (or, if a top-level repeated array is present,
//     the named fields on each element).
//   - neither: print the full response on stdout.
func renderListOutput(cmd *cobra.Command, resp proto.Message,
	items []proto.Message) error {

	ndjson, _ := cmd.Flags().GetBool("ndjson")
	fieldsStr, _ := cmd.Flags().GetString("fields")

	if ndjson && items != nil {
		if fieldsStr != "" {
			fields := strings.Split(fieldsStr, ",")

			return printNDJSONFields(items, fields)
		}

		return printNDJSON(items)
	}

	if fieldsStr != "" {
		fields := strings.Split(fieldsStr, ",")

		return printJSONFields(resp, fields)
	}

	return printJSON(resp)
}
