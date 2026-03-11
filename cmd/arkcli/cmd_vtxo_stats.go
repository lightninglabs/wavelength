package main

import (
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/spf13/cobra"
)

// newVTXOStatsCmd creates the vtxo-stats subcommand.
func newVTXOStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "vtxo-stats",
		Short: "Display aggregate VTXO statistics",
		Long: "Returns total VTXO count, per-status " +
			"counts, and total locked value in sats.",
		RunE: vtxoStatsRun,
	}
}

// vtxoStatsRun executes the vtxo-stats command.
func vtxoStatsRun(cmd *cobra.Command, _ []string) error {
	client, conn, err := getAdminClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &adminrpc.GetVTXOStatsRequest{}
	if err := parseRequest(cmd, req, nil); err != nil {
		return err
	}

	resp, err := client.GetVTXOStats(cmd.Context(), req)
	if err != nil {
		return err
	}

	return printJSON(resp)
}
