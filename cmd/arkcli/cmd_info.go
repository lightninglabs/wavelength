package main

import (
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/spf13/cobra"
)

// newInfoCmd creates the info subcommand.
func newInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Display operator server status",
		Long: "Returns version, pubkey, network, block " +
			"height, and lnd alias for the running " +
			"arkd instance.",
		RunE: infoRun,
	}
}

// infoRun executes the info command.
func infoRun(cmd *cobra.Command, _ []string) error {
	client, conn, err := getAdminClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &adminrpc.InfoRequest{}
	if err := parseRequest(cmd, req, nil); err != nil {
		return err
	}

	resp, err := client.Info(cmd.Context(), req)
	if err != nil {
		return err
	}

	return printJSON(resp)
}
