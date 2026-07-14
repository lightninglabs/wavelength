package main

import (
	"os"

	"github.com/lightninglabs/wavelength/cmd/wavecli/waveclicommands"
)

// main runs the wavecli root command and maps the returned error
// onto a semantic exit code so agents can branch on the failure
// category without parsing prose. See waveclicommands/exit_codes.go
// for the full table (2=invalid args, 3=auth, 4=not found, 10=dry-run
// passed). Any error that already carries the structured envelope set
// by helpers like PrintError is treated as already-rendered; otherwise
// we emit a normalized error envelope so stderr stays machine-readable
// in every failure path.
func main() {
	root := waveclicommands.NewRootCmd()

	err := root.Execute()
	if err == nil {
		return
	}

	if !waveclicommands.ErrorWasPrinted(err) {
		_ = waveclicommands.PrintCommandError(err)
	}

	os.Exit(waveclicommands.ExitCodeFor(err))
}
