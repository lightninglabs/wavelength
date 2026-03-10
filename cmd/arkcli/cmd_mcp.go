package main

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/build"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// newMCPCmd creates the mcp parent command.
func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "MCP server operations",
		Long: "Model Context Protocol (MCP) server that " +
			"exposes the admin RPC as typed tool " +
			"calls over stdio.",
	}

	cmd.AddCommand(newMCPServeCmd())

	return cmd
}

// newMCPServeCmd creates the mcp serve subcommand.
func newMCPServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start MCP stdio server",
		Long: "Starts an MCP server on stdio that exposes " +
			"each admin RPC as a typed tool call. " +
			"Agents invoke tools with structured " +
			"JSON parameters; responses are the raw " +
			"proto-JSON from the daemon.",
		RunE: mcpServe,
	}
}

// mcpServe starts the MCP stdio server.
func mcpServe(cmd *cobra.Command, _ []string) error {
	client, conn, err := getAdminClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "arkcli",
			Version: build.Version(),
		},
		nil,
	)

	// Register all admin RPC tools.
	registerMCPTools(server, client)

	// Run on stdio transport until the client disconnects.
	return server.Run(
		context.Background(), &mcp.StdioTransport{},
	)
}

// mcpResult builds a CallToolResult from a proto message response.
func mcpResult(msg proto.Message) (*mcp.CallToolResult, error) {
	opts := protojson.MarshalOptions{
		Indent:          "  ",
		UseProtoNames:   true,
		EmitUnpopulated: true,
	}

	data, err := opts.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal response: %w", err)
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(data)},
		},
	}, nil
}

// registerMCPTools adds all admin RPC methods as MCP tools.
func registerMCPTools(s *mcp.Server,
	client adminrpc.OperatorAdminClient) {

	// info — no parameters.
	type infoArgs struct{}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "info",
		Description: "Display operator server status",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		_ infoArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.Info(
			ctx, &adminrpc.InfoRequest{},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// trigger_batch — no parameters.
	type triggerBatchArgs struct{}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "trigger_batch",
		Description: "Manually trigger a new batch round",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		_ triggerBatchArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.TriggerBatch(
			ctx, &adminrpc.TriggerBatchRequest{},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// list_rounds — with optional filters.
	type listRoundsArgs struct {
		StatusFilter string `json:"status_filter,omitempty"`
		Limit        uint32 `json:"limit,omitempty"`
		Offset       uint32 `json:"offset,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_rounds",
		Description: "List past and active rounds",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		args listRoundsArgs) (*mcp.CallToolResult, any, error) {

		rpcReq := &adminrpc.ListRoundsRequest{
			Limit:  args.Limit,
			Offset: args.Offset,
		}

		if args.StatusFilter != "" {
			status, ok := parseRoundStatus(
				args.StatusFilter,
			)
			if !ok {
				return nil, nil, fmt.Errorf(
					"invalid status: %s",
					args.StatusFilter)
			}
			rpcReq.StatusFilter = status
		}

		resp, err := client.ListRounds(ctx, rpcReq)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// list_vtxos — with optional filters.
	type listVTXOsArgs struct {
		StatusFilter string `json:"status_filter,omitempty"`
		Limit        uint32 `json:"limit,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_vtxos",
		Description: "List VTXOs with optional filters",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		args listVTXOsArgs) (*mcp.CallToolResult, any, error) {

		rpcReq := &adminrpc.ListVTXOsRequest{
			Limit: args.Limit,
		}

		if args.StatusFilter != "" {
			status, ok := parseVTXOStatus(
				args.StatusFilter,
			)
			if !ok {
				return nil, nil, fmt.Errorf(
					"invalid status: %s",
					args.StatusFilter)
			}
			rpcReq.StatusFilter = []adminrpc.VTXOStatus{
				status,
			}
		}

		resp, err := client.ListVTXOs(ctx, rpcReq)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// vtxo_stats — no parameters.
	type vtxoStatsArgs struct{}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "vtxo_stats",
		Description: "Display aggregate VTXO statistics",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		_ vtxoStatsArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.GetVTXOStats(
			ctx, &adminrpc.GetVTXOStatsRequest{},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// list_clients — no parameters.
	type listClientsArgs struct{}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_clients",
		Description: "List registered mailbox clients",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		_ listClientsArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.ListClients(
			ctx, &adminrpc.ListClientsRequest{},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})
}
