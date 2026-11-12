package waveclicommands

import (
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/spf13/cobra"
)

// newExitBatchCmd exposes one cooperative batch without changing singular Exit.
func newExitBatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "batch",
		Short: "Cooperatively exit selected VTXOs in one request",
		Long: "Queues one cooperative intent for selected inputs. " +
			"Inputs may originate in different rounds. " +
			"Each creates an output to the common destination. " +
			"Partial admission is possible. Reconcile unknown " +
			"outcomes before resubmitting.",
		Args: cobra.NoArgs,
		RunE: walletExitBatch,
	}
	cmd.Flags().StringArray("outpoint", nil,
		"selected VTXO (txid:vout); repeat for each input")
	_ = cmd.MarkFlagRequired("outpoint")
	cmd.Flags().String("onchain-address", "",
		"common destination; omitted allocates one wallet-owned "+
			"address")
	cmd.Flags().Bool("dry-run", false,
		"validate locally and print request without dispatching")

	return cmd
}

// walletExitBatch prints every result before reporting partial or unknown
// admission as a command error. It never retries or invokes singular Exit.
func walletExitBatch(cmd *cobra.Command, _ []string) error {
	targets, _ := cmd.Flags().GetStringArray("outpoint")
	if len(targets) == 0 {
		return invalidArgs(
			fmt.Errorf("at least one --outpoint is required"),
		)
	}
	seen := make(map[string]bool)
	for _, target := range targets {
		if err := invalidArgs(validateOutpoint(target)); err != nil {
			return err
		}
		if seen[target] {
			return invalidArgs(
				fmt.Errorf("duplicate outpoint %s", target),
			)
		}
		seen[target] = true
	}
	address, _ := cmd.Flags().GetString("onchain-address")
	if address != "" {
		if err := invalidArgs(
			validateDestination(address),
		); err != nil {
			return err
		}
	}
	req := &wavewalletrpc.ExitBatchRequest{
		Outpoints:      targets,
		OnchainAddress: address,
	}
	if dryRun, _ := cmd.Flags().GetBool("dry-run"); dryRun {
		return walletDryRunPreview(
			"wavewalletrpc.WalletService/ExitBatch", req,
		)
	}

	accepted := wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_ACCEPTED

	return withWalletClient(cmd,
		func(client wavewalletrpc.WalletServiceClient) error {
			ctx, cancel := rpcContext(cmd)
			defer cancel()

			response, err := client.ExitBatch(ctx, req)
			if err != nil {
				return fmt.Errorf("exit batch: admission may "+
					"be unknown; reconcile before "+
					"resubmitting: %w", err)
			}
			if err := printWalletProto(response); err != nil {
				return err
			}
			for _, input := range response.GetResults() {
				if input.GetOutcome() != accepted {
					return fmt.Errorf("exit batch: some " +
						"inputs were not accepted; " +
						"inspect all results and " +
						"reconcile unknown outcomes " +
						"before resubmitting")
				}
			}
			fmt.Fprintln(
				cmd.ErrOrStderr(),
				"Cooperative inputs queued locally; track "+
					"confirmation with wavecli activity.",
			)

			return nil
		},
	)
}
