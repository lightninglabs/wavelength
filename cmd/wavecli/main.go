package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

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
	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)

	root := waveclicommands.NewRootCmd()

	err := root.ExecuteContext(ctx)
	stop()
	if err == nil {
		return
	}

	if !waveclicommands.ErrorWasPrinted(err) {
		_ = waveclicommands.PrintCommandError(err)
	}

	os.Exit(waveclicommands.ExitCodeFor(err))
}
