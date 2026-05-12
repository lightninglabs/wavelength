//go:build itest

package main

import "github.com/spf13/cobra"

var dataDir string

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "arktest",
		Short: "Local Ark integration harness",
		Long: "arktest starts a local regtest Ark world for manual " +
			"arkd, arkcli, darepod, and darepocli testing. It " +
			"is an itest-only developer helper, not a " +
			"production tool. Each client daemon gets its own " +
			"LND container so unrolls (V3 ephemeral-anchor " +
			"package relay) work against taproot fee inputs.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(
		&dataDir, "datadir", defaultDataDir(),
		"directory for arktest state",
	)

	cmd.AddCommand(
		newStartCmd(), newInfoCmd(), newMineCmd(), newAliasesCmd(),
		newBoardCmd(), newLogsCmd(), newStressCmd(),
	)

	return cmd
}
