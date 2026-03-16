package darepoclicommands

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
)

// newOORCmd creates the oor parent command with receive-target subcommands.
func newOORCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "oor",
		Short: "Out-of-round operations",
		Long: "Manage out-of-round receive targets and other direct " +
			"transfer helpers.",
	}

	cmd.AddCommand(
		newOORReceiveCmd(),
	)

	return cmd
}

// newOORReceiveCmd creates the oor receive subcommand.
func newOORReceiveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "receive",
		Short: "Allocate a fresh OOR receive script",
		Long: "Allocates a fresh wallet key, registers the matching " +
			"taproot OOR receive script with the indexer, and " +
			"prints the resulting destination details.",
		RunE: oorReceive,
	}

	cmd.Flags().String("label", "",
		"optional label stored with the receive-script registration")

	return cmd
}

// oorReceive executes the NewOORReceiveScript RPC.
func oorReceive(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &daemonrpc.NewOORReceiveScriptRequest{}
	if err := parseRequest(cmd, req, func() error {
		label, _ := cmd.Flags().GetString("label")
		req.Label = label

		return nil
	}); err != nil {
		return err
	}

	resp, err := client.NewOORReceiveScript(
		context.Background(), req,
	)
	if err != nil {
		return fmt.Errorf("NewOORReceiveScript RPC failed: %w", err)
	}

	return printJSON(resp)
}
