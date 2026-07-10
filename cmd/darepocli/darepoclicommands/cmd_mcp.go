package darepoclicommands

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
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

	// The wallet verbs live on the WalletService client; reusing the
	// existing connection keeps TLS, macaroons, --no-tls, and
	// --tlscertpath honored across both surfaces.
	walletClient := walletdkrpc.NewWalletServiceClient(conn)

	server := buildMCPServer(client, walletClient)

	// Run on stdio transport until the client disconnects, honoring the
	// command context so a cancelled invocation (Ctrl+C) shuts the
	// server down.
	return server.Run(
		cmd.Context(), &mcp.StdioTransport{},
	)
}

// buildMCPServer constructs the MCP server and registers every exposed RPC
// as a typed tool. It is separated from mcpServe (which owns the daemon dial
// and the stdio transport) so the advertised tool surface can be
// introspected in tests without a live daemon.
func buildMCPServer(client daemonrpc.DaemonServiceClient,
	walletClient walletdkrpc.WalletServiceClient) *mcp.Server {

	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "darepocli",
			Version: build.Version(),
		},
		nil,
	)

	// Register all RPC tools. The wallet-verb registrations sit
	// above the legacy daemonrpc tools so an agent listing tools
	// sees the everyday surface first.
	registerMCPWalletTools(server, walletClient)
	registerMCPTools(server, client)

	return server
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
			&mcp.TextContent{
				Text: string(data),
			},
		},
	}, nil
}

// registerMCPTools adds all daemon RPC methods as MCP tools.
func registerMCPTools(s *mcp.Server, client daemonrpc.DaemonServiceClient) {
	// getinfo — no parameters.
	type getInfoArgs struct{}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "getinfo",
		Description: "Display daemon status information",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ getInfoArgs) (
		*mcp.CallToolResult, any, error) {

		resp, err := client.GetInfo(
			ctx, &daemonrpc.GetInfoRequest{},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// daemon.balance — no parameters. Uses the daemonrpc.GetBalance
	// shape (boarding + VTXO + total breakdown) and lives under the
	// `daemon.*` namespace so the everyday flat `balance` tool that
	// registerMCPWalletTools registers (walletdkrpc.Balance shape) is
	// not silently overwritten by this richer-but-legacy registration.
	// AddTool replaces tools on name collision, so the two names have
	// to be distinct or the agent surface lies about which shape it
	// returns.
	type balanceArgs struct{}
	mcp.AddTool(s, &mcp.Tool{
		Name: "daemon.balance",
		Description: "Display the daemonrpc balance breakdown " +
			"(boarding + VTXO + total)",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ balanceArgs) (
		*mcp.CallToolResult, any, error) {

		resp, err := client.GetBalance(
			ctx, &daemonrpc.GetBalanceRequest{},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// ark.oor.newaddress — fresh boarding address. Lives under
	// `ark.*` because the top-level `recv --onchain` verb is now
	// the everyday surface; this MCP tool stays for power-user /
	// scripted access to the raw daemonrpc.NewAddress path.
	type arkOORNewAddressArgs struct{}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ark.oor.newaddress",
		Description: "Generate a new boarding address (advanced)",
	}, func(ctx context.Context, req *mcp.CallToolRequest,
		_ arkOORNewAddressArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.NewAddress(
			ctx, &daemonrpc.NewAddressRequest{},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// NOTE: create, unlock, and genseed are intentionally omitted
	// from MCP. These operations handle sensitive material
	// (passwords, seed phrases) that should never transit the MCP
	// protocol where they could leak into agent logs or provider
	// APIs. Use the CLI directly for wallet setup. See #164 and
	// #165 for secure agent wallet init plans.

	// receive_script — fresh receive script.
	type receiveScriptArgs struct {
		Label string `json:"label,omitempty" jsonschema:"optional indexer registration label"` //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ark.oor.receive",
		Description: "Allocate a fresh receive script",
	}, func(ctx context.Context, req *mcp.CallToolRequest,
		args receiveScriptArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.NewReceiveScript(
			ctx, &daemonrpc.NewReceiveScriptRequest{
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
		StatusFilter string `json:"status_filter,omitempty" jsonschema:"VTXO status filter (live, pending_forfeit, unilateral_exit, etc.)"` //nolint:ll
		MinAmountSat int64  `json:"min_amount_sat,omitempty" jsonschema:"minimum amount in sats"`                                           //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ark.vtxos.list",
		Description: "List VTXOs with optional filters",
	}, func(ctx context.Context, req *mcp.CallToolRequest,
		args vtxosListArgs) (*mcp.CallToolResult, any, error) {

		rpcReq := &daemonrpc.ListVTXOsRequest{
			MinAmountSat: args.MinAmountSat,
		}

		if args.StatusFilter != "" {
			status, ok := parseVTXOStatus(
				args.StatusFilter,
			)
			if !ok {
				return nil, nil, fmt.Errorf("invalid "+
					"status: %s", args.StatusFilter)
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
		Name:        "ark.vtxos.refresh",
		Description: "Queue VTXOs for refresh in next round",
	}, func(ctx context.Context, req *mcp.CallToolRequest,
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

	// vtxos_leave — cooperative leave (offboard).
	type vtxosLeaveArgs struct {
		Outpoints    []string          `json:"outpoints,omitempty" jsonschema:"VTXO outpoint(s) to leave (txid:index)"`                                     //nolint:ll
		All          bool              `json:"all,omitempty" jsonschema:"leave all live VTXOs"`                                                             //nolint:ll
		Address      string            `json:"address,omitempty" jsonschema:"default on-chain destination address"`                                         //nolint:ll
		PkScript     string            `json:"pk_script,omitempty" jsonschema:"default destination pk_script as hex; alternative to address"`               //nolint:ll
		Destinations map[string]string `json:"destinations,omitempty" jsonschema:"per-outpoint override map; values are either an address or script:<hex>"` //nolint:ll
		DryRun       bool              `json:"dry_run,omitempty" jsonschema:"validate without queuing"`                                                     //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ark.vtxos.leave",
		Description: "Queue VTXOs for cooperative leave (offboard)",
	}, func(ctx context.Context, req *mcp.CallToolRequest,
		args vtxosLeaveArgs) (*mcp.CallToolResult, any, error) {

		// Reuse the CLI's builder so CLI and MCP can't drift on
		// destination parsing — a divergence would let the two
		// surfaces offboard to subtly different scripts.
		rpcReq, err := buildLeaveVTXOsRequest(
			args.Outpoints, args.All, args.Address, args.PkScript,
			args.Destinations, args.DryRun,
		)
		if err != nil {
			return nil, nil, err
		}

		resp, err := client.LeaveVTXOs(ctx, rpcReq)
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
func registerMCPSendTools(s *mcp.Server, client daemonrpc.DaemonServiceClient) {
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
		Name:        "ark.send.inround",
		Description: "Send via in-round refresh",
	}, func(ctx context.Context, req *mcp.CallToolRequest,
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
		Address        string `json:"address,omitempty" jsonschema:"recipient address (mutually exclusive with pubkey_xonly_hex)"`                   //nolint:ll
		PubKeyXOnlyHex string `json:"pubkey_xonly_hex,omitempty" jsonschema:"recipient 32-byte x-only pubkey hex (mutually exclusive with address)"` //nolint:ll
		AmountSat      int64  `json:"amount_sat" jsonschema:"amount in sats"`                                                                        //nolint:ll
		DryRun         bool   `json:"dry_run,omitempty" jsonschema:"validate without initiating"`                                                    //nolint:ll
		IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"stable caller intent key for retry-safe OOR sends"`                      //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ark.send.oor",
		Description: "Send via out-of-round transfer",
	}, func(ctx context.Context, req *mcp.CallToolRequest,
		args sendOORArgs) (*mcp.CallToolResult, any, error) {

		recipient, err := buildOORRecipientOutput(
			args.Address, args.PubKeyXOnlyHex, args.AmountSat,
		)
		if err != nil {
			return nil, nil, err
		}

		resp, err := client.SendOOR(
			ctx, &daemonrpc.SendOORRequest{
				Recipients:     []*daemonrpc.Output{recipient},
				DryRun:         args.DryRun,
				IdempotencyKey: args.IdempotencyKey,
			},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})
}
