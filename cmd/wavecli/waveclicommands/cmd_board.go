package waveclicommands

import (
	"fmt"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
)

// newBoardCmd creates the board subcommand that triggers round
// registration for confirmed boarding UTXOs.
func newBoardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "board",
		Short: "Board confirmed UTXOs into VTXOs",
		Long: "Triggers the client to join the next " +
			"round with any confirmed boarding UTXOs. " +
			"The resulting VTXO set replaces the on-chain " +
			"boarding balance. By default the whole balance " +
			"is boarded into one VTXO.",
		RunE: board,
	}

	cmd.Flags().Uint32(
		"target-vtxo-count", 0,
		"number of VTXOs to fan the boarded balance into",
	)

	cmd.Flags().Bool(
		"no-persist", false,
		"opt out of restart-safe Board replay: do not persist "+
			"the Board intent, so a daemon restart between "+
			"admission and round seal silently drops it",
	)

	return cmd
}

// board executes the Board RPC and prints the result.
func board(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &waverpc.BoardRequest{}
	if err := parseRequest(cmd, req, func() error {
		count, _ := cmd.Flags().GetUint32("target-vtxo-count")
		req.TargetVtxoCount = count

		noPersist, _ := cmd.Flags().GetBool("no-persist")
		req.NoPersist = noPersist

		return nil
	}); err != nil {
		return err
	}

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	resp, err := client.Board(ctx, req)
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
