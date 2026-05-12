package main

import (
	"fmt"
	"strings"

	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

// newListClientsCmd creates the list-clients subcommand.
func newListClientsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list-clients",
		Short: "List registered mailbox clients",
		Long: "Returns the set of currently registered " +
			"mailbox clients with their connection " +
			"status. Supports --ndjson and --fields " +
			"for agent-friendly output.",
		RunE: listClientsRun,
	}

	cmd.Flags().String("fields", "",
		"comma-separated field names to include")
	cmd.Flags().Bool("ndjson", false,
		"emit one JSON object per client (newline-delimited)")

	return cmd
}

// listClientsRun executes the list-clients command.
func listClientsRun(cmd *cobra.Command, _ []string) error {
	client, conn, err := getAdminClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &adminrpc.ListClientsRequest{}
	if err := parseRequest(cmd, req, nil); err != nil {
		return err
	}

	resp, err := client.ListClients(cmd.Context(), req)
	if err != nil {
		return err
	}

	// Apply --fields filtering.
	fieldsStr, _ := cmd.Flags().GetString("fields")
	ndjson, _ := cmd.Flags().GetBool("ndjson")

	if fieldsStr != "" && ndjson {
		return fmt.Errorf("--fields and --ndjson are mutually " +
			"exclusive")
	}

	if fieldsStr != "" {
		fields := strings.Split(fieldsStr, ",")

		return printJSONFields(resp, fields)
	}

	// Emit newline-delimited JSON if --ndjson was specified.
	if ndjson {
		items := make([]proto.Message, len(resp.Clients))
		for i, c := range resp.Clients {
			items[i] = c
		}

		return printNDJSON(items)
	}

	return printJSON(resp)
}
