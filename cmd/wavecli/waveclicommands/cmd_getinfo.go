package waveclicommands

import (
	"fmt"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
)

// newGetInfoCmd creates the getinfo subcommand.
func newGetInfoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "getinfo",
		Short: "Display daemon status information",
		Long: "Returns version, network, wallet state, " +
			"block height, and identity pubkey from " +
			"the running daemon.",
		RunE: getInfo,
	}

	return cmd
}

// getInfo executes the GetInfo RPC and prints the result.
func getInfo(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &waverpc.GetInfoRequest{}
	if err := parseRequest(cmd, req, nil); err != nil {
		return err
	}

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	resp, err := client.GetInfo(ctx, req)
	if err != nil {
		return fmt.Errorf("GetInfo RPC failed: %w", err)
	}

	return printJSON(resp)
}
