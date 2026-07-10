package darepoclicommands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/cmd/darepocli/darepoclicommands/devrpc"
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/spf13/cobra"
)

const (
	// defaultRPCServer is the default gRPC endpoint for the daemon.
	defaultRPCServer = "localhost:10029"
)

// NewRootCmd creates the top-level cobra command for darepocli. Global
// flags (--rpcserver, --format, --tlscertpath, --macaroonpath, --no-tls) are
// registered here and made available to all subcommands via PersistentFlags.
func NewRootCmd() *cobra.Command {
	defaultCfg := darepod.DefaultConfig()
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

	pf.String(
		"datadir", defaultCfg.DataDir,
		"root data directory for daemon state",
	)

	pf.String(
		"network", defaultCfg.Network,
		"bitcoin network for default TLS/macaroon paths",
	)

	pf.String("tlscertpath", "",
		"path to daemon TLS certificate")

	pf.String("macaroonpath", "",
		"path to daemon RPC macaroon")

	pf.Bool(
		"no-tls", false,
		"disable TLS for daemon connection (dev/regtest)",
	)

	pf.Bool(
		"no-macaroons", false, "disable macaroon authentication "+
			"for daemon connection (dev/regtest)",
	)

	pf.String(
		"json", "", "raw JSON request payload (maps directly to "+
			"the RPC request proto); when set, bespoke flags "+
			"are ignored",
	)

	// Register subcommands. The implicit top-level wallet verbs
	// (create / unlock / send / recv / activity / balance / exit /
	// wallet-sweep) are the face of the CLI; everything else groups under
	// the named `ark` parent or stays at root for daemon introspection.
	cmd.AddCommand(
		// Wallet verbs (top-level / implicit).
		newCreateCmd(),
		newUnlockCmd(),
		newSendCmd(),
		newRecvCmd(),
		newActivityCmd(),
		newBalanceCmd(),
		newExitCmd(),
		newWalletSweepCmd(),

		// Daemon introspection at root.
		newGetInfoCmd(),
		newSchemaCmd(),
		newMCPCmd(),

		// Advanced subtrees.
		newArkCmd(),
		newRecoveryCmd(),

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

type errorEnvelope struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// PrintError writes a structured error to stderr in JSON format and
// returns an error that carries the same code/message plus a marker
// that main() can pick up via ErrorWasPrinted to avoid re-emitting the
// envelope. Callers `return PrintError(...)` from RunE so the cobra
// surface keeps signalling failure to the harness while stderr stays
// machine-readable.
func PrintError(code string, msg string) error {
	if err := printErrorDetails(os.Stderr, code, msg, ""); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", msg)
	}

	return &printedError{code: code, msg: msg}
}

// PrintErrorDetails writes a structured error to stderr with optional
// diagnostic context for the message.
func PrintErrorDetails(code string, msg string, details string) error {
	if err := printErrorDetails(os.Stderr, code, msg, details); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", msg)
	}

	return &printedError{code: code, msg: msg, details: details}
}

func printError(w io.Writer, code string, msg string) error {
	return printErrorDetails(w, code, msg, "")
}

func printErrorDetails(w io.Writer, code string, msg string,
	details string) error {

	data, err := json.MarshalIndent(errorEnvelope{
		Error: errorPayload{
			Code:    code,
			Message: msg,
			Details: details,
		},
	}, "", "  ")
	if err != nil {
		return err
	}

	_, err = fmt.Fprintln(w, string(data))

	return err
}

// printedError is the wrapper returned by PrintError. It carries a
// semantic exit code derived from the error code string so the binary
// surfaces validation / auth / not-found failures distinctly without
// callers needing to thread codes manually.
type printedError struct {
	code    string
	msg     string
	details string
}

// Error returns the underlying message for cobra's RunE plumbing.
func (e *printedError) Error() string {
	return e.msg
}

// ExitCode maps the textual error code onto a semantic exit code. The
// known prefixes line up with the agent-cli table; everything else
// falls through to the generic failure code.
func (e *printedError) ExitCode() int {
	switch e.code {
	case "INVALID_ARGS", "INVALID_STATUS", "INVALID_OUTPOINT",
		"INVALID_DESTINATION", "INVALID_VIEW":
		return ExitInvalidArgs

	case "AUTH_FAILURE", "WALLET_LOCKED":
		return ExitAuthFailure

	case "METHOD_NOT_FOUND", "NOT_FOUND":
		return ExitNotFound

	case "DRY_RUN_OK":
		return ExitDryRunOK
	}

	return ExitGenericError
}

// ErrorWasPrinted reports whether err originated from PrintError and
// therefore already produced a structured stderr envelope. main()
// uses this to suppress its fallback EXECUTION_FAILED emission for
// errors that have already been rendered.
func ErrorWasPrinted(err error) bool {
	if err == nil {
		return false
	}

	var pe *printedError

	return errors.As(err, &pe)
}
