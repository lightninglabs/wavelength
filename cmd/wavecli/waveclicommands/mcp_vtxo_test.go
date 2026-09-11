package waveclicommands

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// recordingVTXOClient captures the inventory request sent by the MCP tool.
type recordingVTXOClient struct {
	waverpc.DaemonServiceClient

	requests []*waverpc.ListVTXOsRequest
}

// ListVTXOs records the request and returns an empty inventory.
func (c *recordingVTXOClient) ListVTXOs(_ context.Context,
	req *waverpc.ListVTXOsRequest, _ ...grpc.CallOption) (
	*waverpc.ListVTXOsResponse, error) {

	c.requests = append(c.requests, req)

	return &waverpc.ListVTXOsResponse{}, nil
}

// TestMCPVTXOAssetFilter checks the live tool schema and RPC forwarding.
func TestMCPVTXOAssetFilter(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rpcClient := &recordingVTXOClient{}
	server := buildMCPServer(rpcClient, nil)
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

	tools, err := clientSession.ListTools(ctx, nil)
	require.NoError(t, err)

	var schema []byte
	for _, tool := range tools.Tools {
		if tool.Name == "ark.vtxos.list" {
			schema, err = json.Marshal(tool.InputSchema)
			require.NoError(t, err)
		}
	}
	require.Contains(t, string(schema), `"asset_ref"`)

	assetRef := "asset-id:" + strings.Repeat("01", 32)
	for i, ref := range []string{"", assetRef} {
		args := make(map[string]any)
		if ref != "" {
			args["asset_ref"] = ref
		}

		result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
			Name:      "ark.vtxos.list",
			Arguments: args,
		})
		require.NoError(t, err)
		require.False(t, result.IsError)
		require.Len(t, rpcClient.requests, i+1)
		require.Equal(t, ref, rpcClient.requests[i].AssetRef)
	}
}
