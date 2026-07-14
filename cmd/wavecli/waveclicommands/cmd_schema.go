package waveclicommands

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newSchemaCmd creates the schema introspection command. Agents use
// this to self-serve API documentation at runtime instead of relying
// on static docs baked into system prompts.
func newSchemaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema [method]",
		Short: "Dump method schema as JSON",
		Long: "Returns the full method signature for a " +
			"CLI command as machine-readable JSON: " +
			"params, types, required fields, enum " +
			"values, request/response types. Use " +
			"--all to dump every method.",
		Args: cobra.MaximumNArgs(1),
		RunE: schemaRun,
	}

	cmd.Flags().Bool("all", false,
		"dump all method schemas")

	return cmd
}

// schemaRun implements the schema command.
func schemaRun(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool("all")
	registry := methodRegistry()

	if all {
		return printRawJSON(registry)
	}

	if len(args) == 0 {
		// List available method names.
		names := make([]string, 0, len(registry))
		for _, m := range registry {
			names = append(names, m.Method)
		}

		return printRawJSON(names)
	}

	methodName := args[0]
	for _, m := range registry {
		if m.Method == methodName {
			return printRawJSON(m)
		}
	}

	return PrintError(
		"METHOD_NOT_FOUND", fmt.Sprintf("unknown method %q; run "+
			"'wavecli schema' to list available methods",
			methodName),
	)
}
