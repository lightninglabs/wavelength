package main

import (
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/spf13/cobra"
)

// newTriggerBatchCmd creates the trigger-batch subcommand.
func newTriggerBatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trigger-batch",
		Short: "Manually trigger a new batch round",
		Long: "Sends a signal to the rounds subsystem to " +
			"start a new batch round. If a round is " +
			"already in progress, the request is rejected.",
		RunE: triggerBatchRun,
	}

	return cmd
}

// triggerBatchRun executes the trigger-batch command.
func triggerBatchRun(cmd *cobra.Command, _ []string) error {
	client, conn, err := getAdminClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &adminrpc.TriggerBatchRequest{}
	if err := parseRequest(cmd, req, nil); err != nil {
		return err
	}

	resp, err := client.TriggerBatch(cmd.Context(), req)
	if err != nil {
		return err
	}

	return printJSON(resp)
}
