package main

import (
	"fmt"
	"strings"

	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

// newListVTXOsCmd creates the list-vtxos subcommand.
func newListVTXOsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list-vtxos",
		Short: "List VTXOs with optional filters",
		Long: "Returns a paginated list of VTXOs with " +
			"optional status filtering. Supports " +
			"--ndjson and --fields for agent-friendly " +
			"output.",
		RunE: listVTXOsRun,
	}

	cmd.Flags().String("status", "",
		"filter by VTXO status (pending, live, in_flight, "+
			"forfeited, spent, unrolled_by_client, expired)")
	cmd.Flags().Uint32("limit", 0,
		"maximum number of VTXOs to return")
	cmd.Flags().String("fields", "",
		"comma-separated field names to include")
	cmd.Flags().Bool("ndjson", false,
		"emit one JSON object per VTXO (newline-delimited)")

	return cmd
}

// listVTXOsRun executes the list-vtxos command.
func listVTXOsRun(cmd *cobra.Command, _ []string) error {
	client, conn, err := getAdminClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &adminrpc.ListVTXOsRequest{}
	err = parseRequest(cmd, req, func() error {
		limit, _ := cmd.Flags().GetUint32("limit")
		statusStr, _ := cmd.Flags().GetString("status")

		req.Limit = limit

		if statusStr != "" {
			status, ok := parseVTXOStatus(statusStr)
			if !ok {
				return fmt.Errorf(
					"invalid VTXO status: %s",
					statusStr)
			}
			req.StatusFilter = []adminrpc.VTXOStatus{
				status,
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	resp, err := client.ListVTXOs(cmd.Context(), req)
	if err != nil {
		return err
	}

	// Apply --fields filtering.
	fieldsStr, _ := cmd.Flags().GetString("fields")
	ndjson, _ := cmd.Flags().GetBool("ndjson")

	if fieldsStr != "" && ndjson {
		return fmt.Errorf(
			"--fields and --ndjson are " +
				"mutually exclusive",
		)
	}

	if fieldsStr != "" {
		fields := strings.Split(fieldsStr, ",")
		return printJSONFields(resp, fields)
	}

	// Emit newline-delimited JSON if --ndjson was specified.
	if ndjson {
		items := make([]proto.Message, len(resp.Vtxos))
		for i, v := range resp.Vtxos {
			items[i] = v
		}

		return printNDJSON(items)
	}

	return printJSON(resp)
}

// parseVTXOStatus maps a string to VTXOStatus enum.
func parseVTXOStatus(s string) (adminrpc.VTXOStatus, bool) {
	switch strings.ToLower(s) {
	case "pending":
		return adminrpc.VTXOStatus_VTXO_STATUS_PENDING, true

	case "live":
		return adminrpc.VTXOStatus_VTXO_STATUS_LIVE, true

	case "forfeited":
		return adminrpc.VTXOStatus_VTXO_STATUS_FORFEITED, true

	case "in_flight":
		return adminrpc.VTXOStatus_VTXO_STATUS_IN_FLIGHT, true

	case "spent":
		return adminrpc.VTXOStatus_VTXO_STATUS_SPENT, true

	case "unrolled_by_client":
		return adminrpc.VTXOStatus_VTXO_STATUS_UNROLLED_BY_CLIENT, true

	case "expired":
		return adminrpc.VTXOStatus_VTXO_STATUS_EXPIRED, true

	default:
		return adminrpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED,
			false
	}
}
