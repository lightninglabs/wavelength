package main

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

	// wallet_create — InitWalletRequest as JSON input.
	type walletCreateArgs struct {
		Mnemonic       []string `json:"mnemonic" jsonschema:"the aezeed mnemonic words"`
		WalletPassword string   `json:"wallet_password" jsonschema:"wallet password (base64-encoded bytes)"`
		SeedPassphrase string   `json:"seed_passphrase,omitempty" jsonschema:"optional aezeed passphrase"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "wallet_create",
		Description: "Create wallet from mnemonic and password",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		args walletCreateArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.InitWallet(
			ctx, &daemonrpc.InitWalletRequest{
				Mnemonic:       args.Mnemonic,
				WalletPassword: []byte(args.WalletPassword),
				SeedPassphrase: []byte(args.SeedPassphrase),
			},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// wallet_unlock — password input.
	type walletUnlockArgs struct {
		WalletPassword string `json:"wallet_password" jsonschema:"wallet password"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "wallet_unlock",
		Description: "Unlock an existing wallet",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		args walletUnlockArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.UnlockWallet(
			ctx, &daemonrpc.UnlockWalletRequest{
				WalletPassword: []byte(
					args.WalletPassword,
				),
			},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// wallet_genseed — generate a new seed.
	type genSeedArgs struct {
		SeedPassphrase string `json:"seed_passphrase,omitempty" jsonschema:"optional aezeed passphrase"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "wallet_genseed",
		Description: "Generate a new aezeed mnemonic",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		args genSeedArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.GenSeed(
			ctx, &daemonrpc.GenSeedRequest{
				SeedPassphrase: []byte(
					args.SeedPassphrase,
				),
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
		StatusFilter string `json:"status_filter,omitempty" jsonschema:"VTXO status filter (live, spent, expiring, etc.)"`
		MinAmountSat int64  `json:"min_amount_sat,omitempty" jsonschema:"minimum amount in sats"`
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
		Outpoints []string `json:"outpoints,omitempty" jsonschema:"VTXO outpoint(s) to refresh (txid:index)"`
		All       bool     `json:"all,omitempty" jsonschema:"refresh all live VTXOs"`
		DryRun    bool     `json:"dry_run,omitempty" jsonschema:"validate without queuing"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "vtxos_refresh",
		Description: "Queue VTXOs for refresh in next round",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		args vtxosRefreshArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.RefreshVTXOs(
			ctx, &daemonrpc.RefreshVTXOsRequest{
				Outpoints: args.Outpoints,
				All:       args.All,
				DryRun:    args.DryRun,
			},
		)
		if err != nil {
			return nil, nil, err
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	// send_inround — in-round send.
	type sendRecipient struct {
		Address   string `json:"address" jsonschema:"recipient address"`
		AmountSat int64  `json:"amount_sat" jsonschema:"amount in sats"`
	}
	type sendInRoundArgs struct {
		Recipients []sendRecipient `json:"recipients" jsonschema:"list of recipients"`
		DryRun     bool            `json:"dry_run,omitempty" jsonschema:"validate without submitting"`
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
					Address:   r.Address,
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
		Address   string `json:"address" jsonschema:"recipient address"`
		AmountSat int64  `json:"amount_sat" jsonschema:"amount in sats"`
		DryRun    bool   `json:"dry_run,omitempty" jsonschema:"validate without initiating"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "send_oor",
		Description: "Send via out-of-round transfer",
	}, func(ctx context.Context,
		req *mcp.CallToolRequest,
		args sendOORArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.SendOOR(
			ctx, &daemonrpc.SendOORRequest{
				Address:   args.Address,
				AmountSat: args.AmountSat,
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

