package main

import (
	"fmt"
	"os"

	"github.com/lightninglabs/darepo-client/build"
	"github.com/spf13/cobra"
)

const (
	// defaultRPCServer is the default gRPC endpoint for the daemon.
	defaultRPCServer = "localhost:10029"
)

func main() {
	root := newRootCmd()

	if err := root.Execute(); err != nil {
		printError("EXECUTION_FAILED", err.Error())
		os.Exit(1)
	}
}

// newRootCmd creates the top-level cobra command for darepocli. Global
// flags (--rpcserver, --format, --tlscertpath, --no-tls) are registered
// here and made available to all subcommands via PersistentFlags.
func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "darepocli",
		Short: "Ark client daemon CLI",
		Long: "darepocli is the command-line interface for " +
			"the Ark protocol client daemon (darepod). " +
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

	pf.String("rpcserver", defaultRPCServer,
		"daemon gRPC server address (host:port)")

	pf.String("tlscertpath", "",
		"path to daemon TLS certificate")

	pf.String("json", "",
		"raw JSON request payload (maps directly to the "+
			"RPC request proto); when set, bespoke flags "+
			"are ignored")

	// Register subcommands.
	cmd.AddCommand(
		newGetInfoCmd(),
		newWalletCmd(),
		newVTXOsCmd(),
		newSendCmd(),
	)

	return cmd
}

// printError writes a structured error to stderr in JSON format.
func printError(code string, msg string) {
	fmt.Fprintf(os.Stderr,
		`{"error":{"code":%q,"message":%q}}`+"\n",
		code, msg)
}
