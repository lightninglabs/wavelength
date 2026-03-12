package main

import (
	"os"

	"github.com/lightninglabs/darepo-client/cmd/darepocli/darepoclicommands"
)

func main() {
	root := darepoclicommands.NewRootCmd()

	if err := root.Execute(); err != nil {
		darepoclicommands.PrintError(
			"EXECUTION_FAILED", err.Error(),
		)
		os.Exit(1)
	}
}
