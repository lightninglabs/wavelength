package darepoclicommands

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
)

// newReceiveCmd creates the generic Ark receive command group.
func newReceiveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "receive",
		Short: "Allocate and inspect Ark receive targets",
		Long: "Allocates Ark receive targets that can be handed to " +
			"senders for in-round or out-of-round transfers.",
		RunE: receive,
	}

	cmd.Flags().String("label", "",
		"optional label stored with the receive-script registration")

	cmd.AddCommand(newReceiveListCmd())

	return cmd
}

// newReceiveListCmd creates the receive list subcommand.
func newReceiveListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered Ark receive targets",
		Long: "Lists receive targets this wallet registered with the " +
			"indexer, including the derived address and pkScript.",
		RunE: receiveList,
	}

	return cmd
}

// receive executes the generic NewReceiveScript RPC wrapper.
func receive(cmd *cobra.Command, _ []string) error {
	return newReceiveScript(cmd)
}

// receiveList executes the ListReceiveScripts RPC.
func receiveList(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	resp, err := client.ListReceiveScripts(
		cmd.Context(), &daemonrpc.ListReceiveScriptsRequest{},
	)
	if err != nil {
		return fmt.Errorf("ListReceiveScripts RPC failed: %w", err)
	}

	return printJSON(resp)
}
