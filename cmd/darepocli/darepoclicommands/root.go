package darepoclicommands

import (
	"fmt"
	"os"

	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/cmd/darepocli/darepoclicommands/devrpc"
	"github.com/spf13/cobra"
)

const (
	// defaultRPCServer is the default gRPC endpoint for the daemon.
	defaultRPCServer = "localhost:10029"
)

// NewRootCmd creates the top-level cobra command for darepocli. Global
// flags (--rpcserver, --format, --tlscertpath, --no-tls) are registered
// here and made available to all subcommands via PersistentFlags.
func NewRootCmd() *cobra.Command {
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

	pf.String(
		"rpcserver", defaultRPCServer,
		"daemon gRPC server address (host:port)",
	)

	pf.String("tlscertpath", "",
		"path to daemon TLS certificate")

	pf.Bool(
		"no-tls", false,
		"disable TLS for daemon connection (dev/regtest)",
	)

	pf.String(
		"json", "", "raw JSON request payload (maps directly to "+
			"the RPC request proto); when set, bespoke flags "+
			"are ignored",
	)

	// Register subcommands. The seven implicit top-level wallet verbs
	// (create / unlock / send / recv / list / balance / exit) are the
	// face of the CLI; everything else groups under named parents
	// (ark / swap) or stays at root for daemon introspection.
	cmd.AddCommand(
		// Wallet verbs (top-level / implicit).
		newCreateCmd(),
		newUnlockCmd(),
		newSendCmd(),
		newRecvCmd(),
		newListCmd(),
		newBalanceCmd(),
		newExitCmd(),

		// Daemon introspection at root.
		newGetInfoCmd(),
		newSchemaCmd(),
		newMCPCmd(),

		// Advanced subtrees.
		newArkCmd(),
		newSwapCmd(),

		devrpc.NewDevCmd(
			devrpc.Config{
				GetConn:     getDaemonConn,
				PrintJSON:   printJSON,
				MapRPCError: mapSwapRuntimeRPCError,
			},
		),
	)

	return cmd
}

// PrintError writes a structured error to stderr in JSON format.
func PrintError(code string, msg string) {
	fmt.Fprintf(
		os.Stderr, `{"error":{"code":%q,"message":%q}}`+"\n", code, msg,
	)
}
