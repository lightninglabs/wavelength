package main

import (
	"os"

	"github.com/lightninglabs/darepo-client/cmd/darepocli/darepoclicommands"
)

// main runs the darepocli root command and maps the returned error
// onto a semantic exit code so agents can branch on the failure
// category without parsing prose. See darepoclicommands/exit_codes.go
// for the full table (2=invalid args, 3=auth, 4=not found, 10=dry-run
// passed). Any error that already carries the structured envelope set
// by helpers like PrintError is treated as already-rendered; otherwise
// we emit a generic EXECUTION_FAILED envelope so stderr stays
// machine-readable in every failure path.
func main() {
	root := darepoclicommands.NewRootCmd()

	err := root.Execute()
	if err == nil {
		return
	}

	if !darepoclicommands.ErrorWasPrinted(err) {
		// Cobra and pflag pre-RunE failures (missing required
		// flag, wrong arg count, unknown flag) come through here
		// as bare errors that should map onto the agent-cli
		// INVALID_ARGS envelope, not the generic EXECUTION_FAILED
		// one. The matching exit-code-2 mapping lives in
		// ExitCodeFor via IsCobraArgError, so the stderr code and
		// the exit code stay in lockstep.
		code := "EXECUTION_FAILED"
		if darepoclicommands.IsCobraArgError(err) {
			code = "INVALID_ARGS"
		}
		_ = darepoclicommands.PrintError(code, err.Error())
	}

	os.Exit(darepoclicommands.ExitCodeFor(err))
}
