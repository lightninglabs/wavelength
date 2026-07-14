package waveclicommands

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// TestSwapCommandRemovedFromRoot verifies that the retired `swap` subtree is
// gone from the root command and that invoking it fails as a clean
// unknown-command argument error (exit code 2) rather than dispatching
// anywhere.
func TestSwapCommandRemovedFromRoot(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	for _, sub := range root.Commands() {
		require.NotEqual(
			t, "swap", sub.Name(),
			"swap subtree should no longer be registered",
		)
	}

	root.SetArgs([]string{"swap", "list"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.Execute()
	require.Error(t, err)
	require.True(
		t, IsCobraArgError(err),
		"unknown command should map to an argument error",
	)
	require.Equal(t, ExitInvalidArgs, ExitCodeFor(err))
}

// TestSwapInvocationHintsAtWalletVerbs verifies that a stale `swap`
// invocation is steered toward its everyday replacements via cobra's
// SuggestFor wiring on `send` and `recv`.
func TestSwapInvocationHintsAtWalletVerbs(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	root.SetArgs([]string{"swap"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.Execute()
	require.Error(t, err)
	require.ErrorContains(t, err, "send")
	require.ErrorContains(t, err, "recv")
}

// TestMethodRegistryDropsSwapMethods verifies that the schema registry no
// longer advertises any swap.* method while the ark.* surface is preserved,
// so agents inspecting the schema are steered to the everyday verbs.
func TestMethodRegistryDropsSwapMethods(t *testing.T) {
	t.Parallel()

	var sawArk bool
	for _, method := range methodRegistry() {
		require.False(
			t, strings.HasPrefix(method.Method, "swap"),
			"schema still lists swap method %q", method.Method,
		)
		if strings.HasPrefix(method.Method, "ark.") {
			sawArk = true
		}
	}

	require.True(t, sawArk, "ark.* methods should remain registered")
}

// TestMCPServerAdvertisesNoSwapTools verifies that the MCP tool surface no
// longer exposes any swap.* tool while the wallet and introspection surfaces
// (send / balance / getinfo) are still advertised. The tool list is
// introspected over an in-memory transport with nil RPC clients, which is
// safe because listing tools never invokes their handlers.
func TestMCPServerAdvertisesNoSwapTools(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	server := buildMCPServer(nil, nil)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()

	client := mcp.NewClient(
		&mcp.Implementation{
			Name:    "test",
			Version: "0",
		},
		nil,
	)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, nil)
	require.NoError(t, err)

	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		require.False(
			t, strings.HasPrefix(tool.Name, "swap."),
			"MCP still advertises swap tool %q", tool.Name,
		)
		names = append(names, tool.Name)
	}

	// Positively pin the wallet surface (registered adjacent to the
	// removed swap registration in buildMCPServer) and daemon
	// introspection, so a regression that dropped either while removing
	// swap fails here rather than shipping green.
	require.Contains(t, names, "send")
	require.Contains(t, names, "balance")
	require.Contains(t, names, "getinfo")
}
