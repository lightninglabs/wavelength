package darepoclicommands

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/daemonrpc"
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
			"exposes the daemon RPC as typed tool " +
			"calls over stdio.",
	}

	cmd.AddCommand(newMCPServeCmd())

	return cmd
}

// newMCPServeCmd creates the mcp serve subcommand.
func newMCPServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start MCP stdio server",
		Long: "Starts an MCP server on stdio that exposes " +
			"each daemon RPC as a typed tool call. " +
			"Agents invoke tools with structured " +
			"JSON parameters; responses are the raw " +
			"proto-JSON from the daemon.",
		RunE: mcpServe,
	}

	return cmd
}

// mcpServe starts the MCP stdio server.
func mcpServe(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "darepocli",
			Version: build.Version(),
		},
		nil,
	)

	// Register all RPC tools.
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

// registerMCPTools adds all daemon RPC methods as MCP tools.
func registerMCPTools(s *mcp.Server,
	client daemonrpc.DaemonServiceClient) {

	// getinfo — no parameters.
	type getInfoArgs struct{}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "getinfo",
		Description: "Display daemon status information",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		_ getInfoArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.GetInfo(
			ctx, &daemonrpc.GetInfoRequest{},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// wallet_balance — no parameters.
	type walletBalanceArgs struct{}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "wallet_balance",
		Description: "Display wallet balance (boarding + VTXO + total)",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		_ walletBalanceArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.GetBalance(
			ctx, &daemonrpc.GetBalanceRequest{},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// wallet_newaddress — no parameters.
	type walletNewAddressArgs struct{}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "wallet_newaddress",
		Description: "Generate a new boarding address",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		_ walletNewAddressArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.NewAddress(
			ctx, &daemonrpc.NewAddressRequest{},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// NOTE: wallet_create, wallet_unlock, and wallet_genseed are
	// intentionally omitted from MCP. These operations handle
	// sensitive material (passwords, seed phrases) that should
	// never transit the MCP protocol where they could leak into
	// agent logs or provider APIs. Use the CLI directly for wallet
	// setup. See #164 and #165 for secure agent wallet init plans.

	// oor_receive — fresh OOR receive script.
	type oorReceiveArgs struct {
		Label string `json:"label,omitempty" jsonschema:"optional indexer registration label"` //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "oor_receive",
		Description: "Allocate a fresh OOR receive script",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		args oorReceiveArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.NewOORReceiveScript(
			ctx, &daemonrpc.NewOORReceiveScriptRequest{
				Label: args.Label,
			},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// vtxos_list — with optional filters.
	type vtxosListArgs struct {
		StatusFilter string `json:"status_filter,omitempty" jsonschema:"VTXO status filter (live, spent, expiring, etc.)"` //nolint:ll
		MinAmountSat int64  `json:"min_amount_sat,omitempty" jsonschema:"minimum amount in sats"`                          //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "vtxos_list",
		Description: "List VTXOs with optional filters",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		args vtxosListArgs) (*mcp.CallToolResult, any, error) {

		rpcReq := &daemonrpc.ListVTXOsRequest{
			MinAmountSat: args.MinAmountSat,
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

			rpcReq.StatusFilter = status
		}

		resp, err := client.ListVTXOs(ctx, rpcReq)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// vtxos_refresh — refresh VTXOs.
	type vtxosRefreshArgs struct {
		Outpoints []string `json:"outpoints,omitempty" jsonschema:"VTXO outpoint(s) to refresh (txid:index)"` //nolint:ll
		All       bool     `json:"all,omitempty" jsonschema:"refresh all live VTXOs"`                         //nolint:ll
		DryRun    bool     `json:"dry_run,omitempty" jsonschema:"validate without queuing"`                   //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "vtxos_refresh",
		Description: "Queue VTXOs for refresh in next round",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		args vtxosRefreshArgs) (*mcp.CallToolResult, any, error) {

		rpcReq := &daemonrpc.RefreshVTXOsRequest{
			DryRun: args.DryRun,
		}
		if args.All {
			rpcReq.Selection = &daemonrpc.RefreshVTXOsRequest_All{
				All: true,
			}
		} else if len(args.Outpoints) > 0 {
			sel := &daemonrpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: args.Outpoints,
				},
			}
			rpcReq.Selection = sel
		}
		resp, err := client.RefreshVTXOs(ctx, rpcReq)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// Register send tools in a separate function to keep each
	// registration function under the line limit.
	registerMCPSendTools(s, client)
}

// registerMCPSendTools adds in-round and out-of-round send tools to
// the MCP server.
func registerMCPSendTools(s *mcp.Server,
	client daemonrpc.DaemonServiceClient) {

	// send_inround — in-round send.
	type sendRecipient struct {
		Address   string `json:"address" jsonschema:"recipient address"`
		AmountSat int64  `json:"amount_sat" jsonschema:"amount in sats"`
	}
	type sendInRoundArgs struct {
		Recipients []sendRecipient `json:"recipients" jsonschema:"list of recipients"`                 //nolint:ll
		DryRun     bool            `json:"dry_run,omitempty" jsonschema:"validate without submitting"` //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "send_inround",
		Description: "Send via in-round refresh",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		args sendInRoundArgs) (*mcp.CallToolResult, any, error) {

		outputs := make(
			[]*daemonrpc.Output, 0,
			len(args.Recipients),
		)
		for _, r := range args.Recipients {
			outputs = append(
				outputs, &daemonrpc.Output{
					Destination: &daemonrpc.Output_Address{
						Address: r.Address,
					},
					AmountSat: r.AmountSat,
				},
			)
		}

		resp, err := client.SendVTXO(
			ctx, &daemonrpc.SendVTXORequest{
				Recipients: outputs,
				DryRun:     args.DryRun,
			},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// send_oor — out-of-round send.
	type sendOORArgs struct {
		Address        string `json:"address,omitempty" jsonschema:"recipient address"`                            //nolint:ll
		PubKeyXOnlyHex string `json:"pubkey_xonly_hex,omitempty" jsonschema:"recipient 32-byte x-only pubkey hex"` //nolint:ll
		PkScriptHex    string `json:"pk_script_hex,omitempty" jsonschema:"recipient raw pk_script hex"`            //nolint:ll
		AmountSat      int64  `json:"amount_sat" jsonschema:"amount in sats"`                                      //nolint:ll
		DryRun         bool   `json:"dry_run,omitempty" jsonschema:"validate without initiating"`                  //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "send_oor",
		Description: "Send via out-of-round transfer",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		args sendOORArgs) (*mcp.CallToolResult, any, error) {

		recipient, err := buildOORRecipientOutput(
			args.Address, args.PubKeyXOnlyHex,
			args.PkScriptHex, args.AmountSat,
		)
		if err != nil {
			return nil, nil, err
		}

		resp, err := client.SendOOR(
			ctx, &daemonrpc.SendOORRequest{
				Recipient: recipient,
				DryRun:    args.DryRun,
			},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})
}
