package waveclicommands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/cmd/wavecli/waveclicommands/devrpc"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/spf13/cobra"
)

const (
	// defaultRPCServer is the default gRPC endpoint for the daemon.
	defaultRPCServer  = "localhost:10029"
	defaultRPCTimeout = 30 * time.Second

	// groupWallet, groupIntrospection, and groupAdvanced are the cobra
	// command-group IDs that shape the default --help face.
	groupWallet        = "wallet"
	groupIntrospection = "introspection"
	groupAdvanced      = "advanced"

	// devModeEnvVar, when set to "1", reveals the advanced subtrees
	// (ark / dev / recovery) under an Advanced group in --help. It only
	// changes visibility; it never gates execution.
	devModeEnvVar = "WAVELENGTH_DEV"
)

// NewRootCmd creates the top-level cobra command for wavecli. Global
// flags (--rpcserver, --timeout, --tlscertpath, --macaroonpath, --no-tls) are
// registered here and made available to all subcommands via PersistentFlags.
//
// The advanced subtrees (ark / dev / recovery) are hidden from the default
// --help unless WAVELENGTH_DEV=1 is set; execution is never gated on the env
// var.
func NewRootCmd() *cobra.Command {
	return newRootCmd(os.Getenv(devModeEnvVar) == "1")
}

// newRootCmd builds the root command with an explicit dev-mode toggle so the
// command surface can be exercised in tests without mutating the process
// environment. When devMode is true the advanced subtrees are revealed under
// an Advanced group; otherwise they are hidden but remain fully runnable.
func newRootCmd(devMode bool) *cobra.Command {
	defaultCfg := waved.DefaultConfig()
	cmd := &cobra.Command{
		Use:   "wavecli",
		Short: "Ark client daemon CLI",
		Long: "wavecli is the command-line interface for " +
			"the Ark protocol client daemon (waved). " +
			"It issues gRPC calls to a running daemon " +
			"and prints command-appropriate human or " +
			"machine-readable output.",
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

	pf.Duration(
		"timeout", defaultRPCTimeout,
		"maximum duration for each daemon RPC; 0 disables the deadline",
	)

	pf.Bool(
		"json", false, "emit machine-readable JSON output (raw "+
			"request input uses --request-json)",
	)

	pf.String(
		"request-json", "", "raw JSON request payload (maps "+
			"directly to the RPC request proto); when set, "+
			"bespoke flags are ignored",
	)

	// The visible face is two groups: the everyday Wallet verbs and
	// daemon Introspection. Register both unconditionally — the built-in
	// help command is assigned to Introspection below, so that group must
	// always exist.
	cmd.AddGroup(
		&cobra.Group{
			ID:    groupWallet,
			Title: "Wallet:",
		}, &cobra.Group{
			ID:    groupIntrospection,
			Title: "Introspection:",
		},
	)

	// Wallet verbs: the day-to-day surface.
	walletCmds := []*cobra.Command{
		newCreateCmd(), newUnlockCmd(), newSendCmd(), newRecvCmd(),
		newActivityCmd(), newBalanceCmd(), newExitCmd(),
		newWalletSweepCmd(),
	}
	for _, c := range walletCmds {
		c.GroupID = groupWallet
	}

	// Daemon introspection at root.
	introspectionCmds := []*cobra.Command{
		newGetInfoCmd(), newSchemaCmd(), newMCPCmd(),
	}
	for _, c := range introspectionCmds {
		c.GroupID = groupIntrospection
	}

	// The advanced subtrees stay compiled and fully runnable in every
	// build; devMode only decides whether they appear in --help. When
	// revealed they sit under an Advanced group; when hidden they carry
	// no GroupID, because cobra prints a group's heading even when all
	// its members are hidden (an empty "Advanced:" would leak) and panics
	// on a GroupID naming a group that was never registered.
	advancedCmds := []*cobra.Command{
		newArkCmd(),
		newRecoveryCmd(),
		devrpc.NewDevCmd(
			devrpc.Config{
				GetConn:     getDaemonConn,
				PrintJSON:   printJSON,
				MapRPCError: mapSwapRuntimeRPCError,
				RPCContext:  rpcContext,
			},
		),
	}
	if devMode {
		cmd.AddGroup(&cobra.Group{
			ID:    groupAdvanced,
			Title: "Advanced:",
		})
	}
	for _, c := range advancedCmds {
		if devMode {
			c.GroupID = groupAdvanced
		} else {
			c.Hidden = true
		}
	}

	// Keep the built-in help and completion commands out of an
	// "Additional Commands" straggler section: group help under
	// Introspection and hide the completion command (it stays runnable).
	cmd.SetHelpCommandGroupID(groupIntrospection)
	cmd.CompletionOptions.HiddenDefaultCmd = true

	cmd.AddCommand(walletCmds...)
	cmd.AddCommand(introspectionCmds...)
	cmd.AddCommand(advancedCmds...)
	cmd.SetGlobalNormalizationFunc(snakeToKebabFlags)

	return cmd
}

type errorEnvelope struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Details     string `json:"details,omitempty"`
	Remediation string `json:"remediation,omitempty"`
	Retryable   bool   `json:"retryable"`
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

	retryable, remediation := errorMetadata(code, msg)

	return printErrorMetadata(
		w, code, msg, details, remediation, retryable,
	)
}

// printErrorMetadata writes one fully classified public error envelope.
func printErrorMetadata(w io.Writer, code, msg, details, remediation string,
	retryable bool) error {

	data, err := json.MarshalIndent(errorEnvelope{
		Error: errorPayload{
			Code:        code,
			Message:     msg,
			Details:     details,
			Remediation: remediation,
			Retryable:   retryable,
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

	case confirmationRequiredCode:
		return ExitConfirmationRequired
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
