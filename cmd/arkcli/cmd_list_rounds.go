package main

import (
	"fmt"
	"strings"

	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

// newListRoundsCmd creates the list-rounds subcommand.
func newListRoundsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list-rounds",
		Short: "List past and active rounds",
		Long: "Returns a paginated list of rounds with " +
			"optional status filtering. Supports " +
			"--ndjson and --fields for agent-friendly " +
			"output.",
		RunE: listRoundsRun,
	}

	cmd.Flags().String("status", "",
		"filter by round status (open, sealed, signing, "+
			"broadcast, confirmed, failed)")
	cmd.Flags().Uint32("limit", 0,
		"maximum number of rounds to return")
	cmd.Flags().Uint32("offset", 0,
		"number of rounds to skip")
	cmd.Flags().String("fields", "",
		"comma-separated field names to include")
	cmd.Flags().Bool("ndjson", false,
		"emit one JSON object per round (newline-delimited)")

	return cmd
}

// listRoundsRun executes the list-rounds command.
func listRoundsRun(cmd *cobra.Command, _ []string) error {
	client, conn, err := getAdminClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &adminrpc.ListRoundsRequest{}
	err = parseRequest(cmd, req, func() error {
		limit, _ := cmd.Flags().GetUint32("limit")
		offset, _ := cmd.Flags().GetUint32("offset")
		statusStr, _ := cmd.Flags().GetString("status")

		req.Limit = limit
		req.Offset = offset

		if statusStr != "" {
			status, ok := parseRoundStatus(statusStr)
			if !ok {
				return fmt.Errorf(
					"invalid round status: %s",
					statusStr)
			}
			req.StatusFilter = status
		}

		return nil
	})
	if err != nil {
		return err
	}

	resp, err := client.ListRounds(cmd.Context(), req)
	if err != nil {
		return err
	}

	// Apply --fields filtering.
	fieldsStr, _ := cmd.Flags().GetString("fields")
	if fieldsStr != "" {
		fields := strings.Split(fieldsStr, ",")
		return printJSONFields(resp, fields)
	}

	// Emit newline-delimited JSON if --ndjson was specified.
	ndjson, _ := cmd.Flags().GetBool("ndjson")
	if ndjson {
		items := make([]proto.Message, len(resp.Rounds))
		for i, r := range resp.Rounds {
			items[i] = r
		}
		return printNDJSON(items)
	}

	return printJSON(resp)
}

// parseRoundStatus maps a string to RoundStatus enum.
func parseRoundStatus(s string) (adminrpc.RoundStatus, bool) {
	switch strings.ToLower(s) {
	case "open":
		return adminrpc.RoundStatus_ROUND_STATUS_OPEN, true

	case "sealed":
		return adminrpc.RoundStatus_ROUND_STATUS_SEALED, true

	case "signing":
		return adminrpc.RoundStatus_ROUND_STATUS_SIGNING, true

	case "broadcast":
		return adminrpc.RoundStatus_ROUND_STATUS_BROADCAST, true

	case "confirmed":
		return adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED, true

	case "failed":
		return adminrpc.RoundStatus_ROUND_STATUS_FAILED, true

	default:
		return adminrpc.RoundStatus_ROUND_STATUS_UNSPECIFIED,
			false
	}
}
