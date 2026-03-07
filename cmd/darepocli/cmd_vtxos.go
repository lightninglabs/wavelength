package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
)

// newVTXOsCmd creates the vtxos parent command with subcommands for
// list and refresh.
func newVTXOsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vtxos",
		Short: "VTXO operations",
		Long: "List, filter, and manage virtual " +
			"transaction outputs (VTXOs).",
	}

	cmd.AddCommand(
		newVTXOsListCmd(),
		newVTXOsRefreshCmd(),
	)

	return cmd
}

// validStatuses lists the valid VTXO status filter values for input
// validation and error messages.
var validStatuses = []string{
	"live", "refresh_requested", "forfeiting",
	"forfeited", "spent", "expiring", "failed",
}

// newVTXOsListCmd creates the vtxos list subcommand.
func newVTXOsListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List VTXOs",
		Long: "Returns VTXOs known to the wallet, " +
			"optionally filtered by status and " +
			"minimum amount.",
		RunE: vtxosList,
	}

	cmd.Flags().String("status", "",
		"filter by status: "+
			strings.Join(validStatuses, ", "))

	cmd.Flags().Int64("min-amount", 0,
		"minimum amount in sats")

	cmd.Flags().String("fields", "",
		"comma-separated field names to include")

	return cmd
}

// vtxosList executes the ListVTXOs RPC with optional filters.
func vtxosList(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &daemonrpc.ListVTXOsRequest{}
	if err := parseRequest(cmd, req, func() error {
		statusStr, _ := cmd.Flags().GetString("status")
		minAmount, _ := cmd.Flags().GetInt64("min-amount")

		if statusStr != "" {
			statusFilter, ok := parseVTXOStatus(
				statusStr,
			)
			if !ok {
				printError("INVALID_STATUS",
					fmt.Sprintf(
						"invalid status %q, "+
							"valid: %s",
						statusStr,
						strings.Join(
							validStatuses,
							", ",
						)))

				return fmt.Errorf(
					"invalid status filter: %s",
					statusStr)
			}

			req.StatusFilter = statusFilter
		}

		req.MinAmountSat = minAmount

		return nil
	}); err != nil {
		return err
	}

	resp, err := client.ListVTXOs(
		context.Background(), req,
	)
	if err != nil {
		return fmt.Errorf("ListVTXOs RPC failed: %w", err)
	}

	return printJSON(resp)
}

// parseVTXOStatus converts a status string to the proto enum.
func parseVTXOStatus(s string) (daemonrpc.VTXOStatus, bool) {
	normalized := strings.ToUpper(s)
	if !strings.HasPrefix(normalized, "VTXO_STATUS_") {
		normalized = "VTXO_STATUS_" + normalized
	}

	val, ok := daemonrpc.VTXOStatus_value[normalized]
	if !ok {
		return 0, false
	}

	return daemonrpc.VTXOStatus(val), true
}

// newVTXOsRefreshCmd creates the vtxos refresh subcommand.
func newVTXOsRefreshCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Queue VTXOs for refresh",
		Long: "Queues one or more VTXOs for refresh in " +
			"the next round, extending their expiry.",
		RunE: vtxosRefresh,
	}

	cmd.Flags().StringSlice("outpoint", nil,
		"VTXO outpoint(s) to refresh (txid:index)")

	cmd.Flags().Bool("all", false,
		"refresh all live VTXOs")

	cmd.Flags().Bool("dry-run", false,
		"validate without queuing")

	return cmd
}

// vtxosRefresh executes the RefreshVTXOs RPC.
func vtxosRefresh(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &daemonrpc.RefreshVTXOsRequest{}
	if err := parseRequest(cmd, req, func() error {
		outpoints, _ := cmd.Flags().GetStringSlice(
			"outpoint",
		)
		all, _ := cmd.Flags().GetBool("all")
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		if !all && len(outpoints) == 0 {
			return fmt.Errorf(
				"either --outpoint or --all " +
					"is required")
		}

		switch {
		case all:
			req.Selection = &daemonrpc.RefreshVTXOsRequest_All{
				All: true,
			}

		default:
			req.Selection = &daemonrpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: outpoints,
				},
			}
		}

		req.DryRun = dryRun

		return nil
	}); err != nil {
		return err
	}

	resp, err := client.RefreshVTXOs(
		context.Background(), req,
	)
	if err != nil {
		return fmt.Errorf(
			"RefreshVTXOs RPC failed: %w", err)
	}

	return printJSON(resp)
}
