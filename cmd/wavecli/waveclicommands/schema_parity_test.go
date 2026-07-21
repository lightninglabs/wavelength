package waveclicommands

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

func TestSchemaRegistryMatchesCobraTree(t *testing.T) {
	t.Parallel()

	root := newRootCmd(true)
	registry := methodRegistry()
	byMethod := make(map[string]schemaMethod, len(registry))

	for _, method := range registry {
		require.NotContains(t, byMethod, method.Method)
		byMethod[method.Method] = method
		require.NotEmpty(t, method.OutputSchemaID)
		require.Equal(t, uint32(1), method.OutputSchemaVersion)

		if method.MCPOnly {
			continue
		}

		cmd, _, err := root.Find(strings.Split(method.Method, "."))
		require.NoError(t, err, method.Method)
		require.Equal(
			t, schemaFlagTypes(method), commandFlagTypes(cmd),
			method.Method,
		)
	}

	walkCommands(root, func(cmd *cobra.Command) {
		method, covered := coveredSchemaMethod(cmd)
		if !covered {
			return
		}

		require.Contains(
			t, byMethod, method, "covered cobra command %q "+
				"lacks schema", cmd.CommandPath(),
		)
	})
}

func TestSchemaRegistryMatchesMCPTools(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	server := buildMCPServer(nil, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()

	client := mcp.NewClient(
		&mcp.Implementation{
			Name:    "schema-parity",
			Version: "0",
		},
		nil,
	)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer clientSession.Close()

	tools, err := clientSession.ListTools(ctx, nil)
	require.NoError(t, err)

	actual := make(map[string]bool, len(tools.Tools))
	for _, tool := range tools.Tools {
		actual[tool.Name] = true
	}

	expected := make(map[string]bool)
	for _, method := range methodRegistry() {
		if method.MCPTool {
			expected[method.Method] = true
		}
	}

	require.Equal(t, expected, actual)
}

func schemaFlagTypes(method schemaMethod) map[string]string {
	flags := make(map[string]string)
	for _, param := range method.Params {
		if param.Positional {
			continue
		}

		flags[param.Name] = schemaParamFlagType(param)
	}

	return flags
}

func schemaParamFlagType(param schemaParam) string {
	if param.FlagType != "" {
		return param.FlagType
	}

	switch param.Type {
	case "enum":
		return "string"

	case "string[]":
		return "stringSlice"

	case "int64[]":
		return "int64Slice"

	case "map[string]string":
		return "stringToString"

	default:
		return param.Type
	}
}

func commandFlagTypes(cmd *cobra.Command) map[string]string {
	flags := make(map[string]string)
	cmd.LocalNonPersistentFlags().VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden || flag.Name == "help" {
			return
		}

		flags[flag.Name] = flag.Value.Type()
	})

	return flags
}

func walkCommands(cmd *cobra.Command, visit func(*cobra.Command)) {
	visit(cmd)
	for _, child := range cmd.Commands() {
		walkCommands(child, visit)
	}
}

func coveredSchemaMethod(cmd *cobra.Command) (string, bool) {
	path := strings.TrimPrefix(cmd.CommandPath(), "wavecli ")
	method := strings.ReplaceAll(path, " ", ".")

	if strings.HasPrefix(path, "ark ") {
		return method, cmd.RunE != nil
	}

	switch method {
	case "create", "unlock", "send", "recv", "activity", "balance",
		"exit", "wallet-sweep", "getinfo", "activity.inspect",
		"exit.status", "exit.summary", "exit.plan":
		return method, true

	default:
		return "", false
	}
}
