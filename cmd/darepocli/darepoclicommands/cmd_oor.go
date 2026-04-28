package darepoclicommands

import (
	"context"
	"fmt"
	"strings"

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
		newOORListCmd(),
		newOORReceiveCmd(),
	)

	return cmd
}

// newOORListCmd creates the oor list subcommand.
func newOORListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List local OOR sessions",
		Long: "Lists locally known out-of-round sessions restored by " +
			"the running daemon.",
		RunE: oorList,
	}

	cmd.Flags().Bool("pending", false,
		"only show sessions that are not completed or failed")
	cmd.Flags().String("direction", "all",
		"session direction: all, outgoing, incoming")

	return cmd
}

// newOORReceiveCmd creates the oor receive subcommand.
func newOORReceiveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "receive",
		Short: "Allocate a fresh receive script",
		Long: "Allocates a fresh wallet key, registers the matching " +
			"taproot receive script with the indexer, and " +
			"prints the resulting destination details.",
		RunE: oorReceive,
	}

	cmd.Flags().String("label", "",
		"optional label stored with the receive-script registration")

	return cmd
}

// oorList executes the ListOORSessions RPC.
func oorList(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	req := &daemonrpc.ListOORSessionsRequest{}
	if err := parseRequest(cmd, req, func() error {
		pendingOnly, _ := cmd.Flags().GetBool("pending")
		direction, _ := cmd.Flags().GetString("direction")

		parsedDirection, err := parseOORSessionDirection(direction)
		if err != nil {
			return err
		}

		req.PendingOnly = pendingOnly
		req.Direction = parsedDirection

		return nil
	}); err != nil {
		return err
	}

	resp, err := client.ListOORSessions(
		context.Background(), req,
	)
	if err != nil {
		return fmt.Errorf("ListOORSessions RPC failed: %w", err)
	}

	return printJSON(resp)
}

// oorReceive executes the NewReceiveScript RPC.
func oorReceive(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	req := &daemonrpc.NewReceiveScriptRequest{}
	if err := parseRequest(cmd, req, func() error {
		label, _ := cmd.Flags().GetString("label")
		req.Label = label

		return nil
	}); err != nil {
		return err
	}

	resp, err := client.NewReceiveScript(
		context.Background(), req,
	)
	if err != nil {
		return fmt.Errorf("NewReceiveScript RPC failed: %w", err)
	}

	return printJSON(resp)
}

// parseOORSessionDirection parses a CLI direction flag into the RPC enum.
func parseOORSessionDirection(
	direction string) (daemonrpc.OORSessionDirection, error) {

	switch strings.ToLower(direction) {
	case "", "all":
		return daemonrpc.
			OORSessionDirection_OOR_SESSION_DIRECTION_ALL, nil
	case "outgoing", "out":
		return daemonrpc.
			OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING, nil
	case "incoming", "in":
		return daemonrpc.
			OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING, nil
	default:
		return daemonrpc.
				OORSessionDirection_OOR_SESSION_DIRECTION_ALL,
			fmt.Errorf("unknown OOR direction %q", direction)
	}
}
