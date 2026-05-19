//go:build !swapruntime

package darepoclicommands

import (
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerMCPSwapTools is a no-op when the binary is built without
// the swapruntime tag: the daemon doesn't expose SwapClientService in
// that mode and registering tools that would always fail with a
// rebuild error is worse for agent DX than simply not listing them.
// CLI consumers still see `swap` in `darepocli --help` because the
// non-swapruntime build registers a placeholder command (see
// cmd_swap_stub.go) that prints the rebuild instruction.
//
// The signature matches the swapruntime variant (typed client
// interface, not a raw conn) so cmd_mcp.go's caller doesn't have to
// branch on the build tag.
func registerMCPSwapTools(_ *mcp.Server,
	_ swapclientrpc.SwapClientServiceClient) {
}
