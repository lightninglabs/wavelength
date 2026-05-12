package main

import (
	"fmt"
	"os"

	"github.com/lightninglabs/darepo/build"
	"github.com/spf13/cobra"
)

const (
	// defaultRPCServer is the default admin gRPC endpoint.
	defaultRPCServer = "localhost:8081"
)

func main() {
	root := newRootCmd()

	if err := root.Execute(); err != nil {
		printError("EXECUTION_FAILED", err.Error())
		os.Exit(1)
	}
}

// newRootCmd creates the top-level cobra command for arkcli. Global
// flags (--rpcserver, --tlscertpath, --no-tls, --json) are registered
// here and made available to all subcommands via PersistentFlags.
func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "arkcli",
		Short: "Ark operator admin CLI",
		Long: "arkcli is the command-line interface for " +
			"the Ark protocol operator daemon (arkd). " +
			"It issues gRPC calls to a running daemon " +
			"and prints structured JSON output suitable " +
			"for both human and agent consumption.",
		Version: build.Version(),
		// Silence usage on errors so we control the error
		// output format.
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Register global persistent flags.
	pf := cmd.PersistentFlags()

	pf.String(
		"rpcserver", defaultRPCServer,
		"admin gRPC server address (host:port)",
	)

	pf.String("tlscertpath", "",
		"path to admin server TLS certificate")

	pf.Bool(
		"no-tls", false,
		"disable TLS for admin connection (dev/regtest)",
	)

	pf.String(
		"json", "", "raw JSON request payload (maps directly to "+
			"the RPC request proto); when set, bespoke flags "+
			"are ignored",
	)

	// Register subcommands.
	cmd.AddCommand(
		newInfoCmd(), newTriggerBatchCmd(), newListRoundsCmd(),
		newListVTXOsCmd(), newVTXOStatsCmd(), newListClientsCmd(),
		newSchemaCmd(), newMCPCmd(),
	)

	return cmd
}

// printError writes a structured error to stderr in JSON format.
func printError(code string, msg string) {
	fmt.Fprintf(
		os.Stderr, `{"error":{"code":%q,"message":%q}}`+"\n", code, msg,
	)
}
