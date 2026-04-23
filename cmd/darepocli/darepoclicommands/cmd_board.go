package darepoclicommands

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
)

// newBoardCmd creates the board subcommand that triggers round
// registration for confirmed boarding UTXOs.
func newBoardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "board",
		Short: "Board confirmed UTXOs into a VTXO",
		Long: "Triggers the client to join the next " +
			"round with any confirmed boarding UTXOs. " +
			"The resulting VTXO replaces the on-chain " +
			"boarding balance.",
		RunE: board,
	}

	return cmd
}

// board executes the Board RPC and prints the result.
func board(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &daemonrpc.BoardRequest{}
	if err := parseRequest(cmd, req, nil); err != nil {
		return err
	}

	resp, err := client.Board(context.Background(), req)
	if err != nil {
		// Map well-known server-side fee rejections to a
		// concise CLI message. Fall through to the generic
		// error wrap if the cause is not a fee rejection.
		if feeErr := mapFeeError(err); feeErr != nil {
			return feeErr
		}

		return fmt.Errorf("board RPC failed: %w", err)
	}

	return printJSON(resp)
}
