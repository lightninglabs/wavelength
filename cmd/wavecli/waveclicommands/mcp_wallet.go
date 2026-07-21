package waveclicommands

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// registerMCPWalletTools registers the everyday wallet verbs on the
// MCP server. Create and Unlock are intentionally omitted: those
// operations carry secrets (wallet password, seed passphrase) that
// must not transit the MCP protocol where they could leak into agent
// logs or provider APIs. Wallet setup stays on the CLI's secret-input
// ladder (WAVED_WALLET_PASSWORD env > --wallet_password_file > stdin
// > TTY prompt).
//
// Each tool maps gRPC Unimplemented to a clear "daemon was not built
// with wavewalletrpc" error so an MCP consumer talking to a stub-build
// daemon sees actionable text rather than a generic status code.
func registerMCPWalletTools(s *mcp.Server,
	client wavewalletrpc.WalletServiceClient) {

	registerMCPWalletQueryTools(s, client)
	registerMCPWalletMutateTools(s, client)
}

// registerMCPWalletQueryTools registers the read-only wallet verbs
// (balance, activity). These never move funds and need no dry-run rail.
func registerMCPWalletQueryTools(s *mcp.Server,
	client wavewalletrpc.WalletServiceClient) {

	type balanceArgs struct{}
	mcp.AddTool(s, &mcp.Tool{
		Name: "balance",
		Description: "Wallet balance: confirmed_sat, " +
			"pending_in_sat, pending_out_sat",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ balanceArgs) (
		*mcp.CallToolResult, any, error) {

		resp, err := client.Balance(
			ctx, &wavewalletrpc.BalanceRequest{},
		)
		if err != nil {
			return nil, nil, mapWalletRPCError(err)
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	type activityArgs struct {
		PendingOnly bool     `json:"pending_only,omitempty" jsonschema:"filter to in-flight entries"`             //nolint:ll
		Kinds       []string `json:"kinds,omitempty" jsonschema:"kind filter (send, recv, deposit, exit)"`        //nolint:ll
		Limit       uint32   `json:"limit,omitempty" jsonschema:"page size; zero uses daemon default"`            //nolint:ll
		Cursor      string   `json:"cursor,omitempty" jsonschema:"activity page token; pass next_cursor to page"` //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "activity",
		Description: "Show wallet activity",
	}, func(ctx context.Context, _ *mcp.CallToolRequest,
		args activityArgs) (*mcp.CallToolResult, any, error) {

		req, err := buildWalletActivityRequest(
			args.PendingOnly, args.Kinds, args.Limit, args.Cursor,
		)
		if err != nil {
			return nil, nil, err
		}

		resp, err := client.List(ctx, req)
		if err != nil {
			return nil, nil, mapWalletRPCError(err)
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	type exitStatusArgs struct {
		Outpoint string `json:"outpoint" jsonschema:"VTXO outpoint to query (txid:vout)"` //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "exit.status",
		Description: "Status of a unilateral exit job",
	}, func(ctx context.Context, _ *mcp.CallToolRequest,
		args exitStatusArgs) (*mcp.CallToolResult, any, error) {

		if err := validateOutpoint(args.Outpoint); err != nil {
			return nil, nil, err
		}
		resp, err := client.ExitStatus(
			ctx, &wavewalletrpc.ExitStatusRequest{
				Outpoint: args.Outpoint,
				Detailed: true,
			},
		)
		if err != nil {
			return nil, nil, mapWalletRPCError(err)
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	type exitSummaryArgs struct{}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "exit.summary",
		Description: "Summarize all in-progress exits and their totals",
	}, func(ctx context.Context, _ *mcp.CallToolRequest,
		_ exitSummaryArgs) (*mcp.CallToolResult, any, error) {

		resp, err := client.ExitSummary(
			ctx, &wavewalletrpc.ExitSummaryRequest{},
		)
		if err != nil {
			return nil, nil, mapWalletRPCError(err)
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})
}

// registerMCPWalletMutateTools registers the mutating wallet verbs
// (send, recv, exit). All three apply the same input-hardening checks
// as the CLI path so MCP and CLI cannot drift on what shapes the
// daemon ever sees.
func registerMCPWalletMutateTools(s *mcp.Server,
	client wavewalletrpc.WalletServiceClient) {

	mcp.AddTool(s, &mcp.Tool{
		Name: "send.prepare",
		Description: "Validate and preview a payment without " +
			"moving funds. Returns a short-lived, single-use " +
			"send_intent_id for the send tool.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest,
		args mcpSendPrepareArgs) (*mcp.CallToolResult, any, error) {

		r, err := prepareMCPWalletSend(ctx, client, args)

		return r, nil, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "send",
		Description: "Dispatch exactly one previously prepared " +
			"payment. Consumes the short-lived, single-use " +
			"send_intent_id and may move funds.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args mcpSendArgs) (
		*mcp.CallToolResult, any, error) {

		r, err := dispatchMCPWalletSend(ctx, client, args)

		return r, nil, err
	})

	type recvArgs struct {
		Direction  string `json:"direction,omitempty" jsonschema:"'offchain' (default) returns a Lightning invoice, 'onchain' returns a boarding address"` //nolint:ll
		AmtSat     uint64 `json:"amt_sat,omitempty" jsonschema:"amount in satoshis (required for offchain)"`                                               //nolint:ll
		Memo       string `json:"memo,omitempty" jsonschema:"optional human-readable memo embedded in the invoice"`                                        //nolint:ll
		AmtSatHint uint64 `json:"amt_sat_hint,omitempty" jsonschema:"optional expected deposit amount for onchain (accounting only)"`                      //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "recv",
		Description: "Receive a payment (offchain Lightning invoice " +
			"by default; onchain boarding address with " +
			"direction='onchain')",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args recvArgs) (
		*mcp.CallToolResult, any, error) {

		offchain, err := parseDirectionField(args.Direction)
		if err != nil {
			return nil, nil, err
		}
		if err := validateFreeText("memo", args.Memo); err != nil {
			return nil, nil, err
		}

		if offchain {
			if args.AmtSat == 0 {
				return nil, nil, fmt.Errorf("amt_sat is " +
					"required for offchain recv")
			}
			resp, err := client.Recv(
				ctx, &wavewalletrpc.RecvRequest{
					AmtSat: args.AmtSat,
					Memo:   args.Memo,
				},
			)
			if err != nil {
				return nil, nil, mapWalletRPCError(err)
			}

			r, err := mcpResult(resp)

			return r, nil, err
		}

		resp, err := client.Deposit(
			ctx, &wavewalletrpc.DepositRequest{
				AmtSatHint: args.AmtSatHint,
			},
		)
		if err != nil {
			return nil, nil, mapWalletRPCError(err)
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	type exitArgs struct {
		Outpoint       string `json:"outpoint" jsonschema:"VTXO outpoint to exit (txid:vout)"`                                 //nolint:ll
		OnchainAddress string `json:"onchain_address,omitempty" jsonschema:"cooperative leave destination; empty uses wallet"` //nolint:ll
		ForceUnrollAck string `json:"force_unroll_ack,omitempty" jsonschema:"exact acknowledgement required for unroll"`       //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "exit",
		Description: "Cooperatively exit a VTXO by default; forced " +
			"unroll requires the exact acknowledgement string",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args exitArgs) (
		*mcp.CallToolResult, any, error) {

		if err := validateOutpoint(args.Outpoint); err != nil {
			return nil, nil, err
		}
		if args.OnchainAddress != "" {
			if err := validateDestination(
				args.OnchainAddress,
			); err != nil {
				return nil, nil, err
			}
		}
		if args.ForceUnrollAck != "" &&
			args.ForceUnrollAck != forceUnrollAck {
			return nil, nil, fmt.Errorf("force_unroll_ack must be "+
				"exactly %q", forceUnrollAck)
		}
		if args.ForceUnrollAck != "" && args.OnchainAddress != "" {
			return nil, nil, fmt.Errorf("onchain_address cannot " +
				"be combined with force_unroll_ack")
		}

		resp, err := client.Exit(
			ctx, &wavewalletrpc.ExitRequest{
				Outpoint:       args.Outpoint,
				OnchainAddress: args.OnchainAddress,
				ForceUnrollAck: args.ForceUnrollAck,
			},
		)
		if err != nil {
			return nil, nil, mapWalletRPCError(err)
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})
}

type mcpSendPrepareArgs struct {
	Destination string `json:"destination" jsonschema:"BOLT-11 invoice (offchain) or onchain address; direction selects the rail"` //nolint:ll
	Direction   string `json:"direction,omitempty" jsonschema:"'offchain' (default) for invoice, 'onchain' for cooperative leave"` //nolint:ll
	AmtSat      uint64 `json:"amt_sat,omitempty" jsonschema:"amount in satoshis (required for onchain unless sweep_all)"`          //nolint:ll
	MaxFeeSat   uint64 `json:"max_fee_sat,omitempty" jsonschema:"max fee in satoshis; zero uses daemon defaults"`                  //nolint:ll
	Note        string `json:"note,omitempty" jsonschema:"caller-supplied label persisted with the entry"`                         //nolint:ll
	SweepAll    bool   `json:"sweep_all,omitempty" jsonschema:"onchain only: drain every live VTXO"`                               //nolint:ll
}

type mcpSendArgs struct {
	SendIntentID string `json:"send_intent_id" jsonschema:"short-lived, single-use id returned by send.prepare"` //nolint:ll
}

// prepareMCPWalletSend validates and quotes a send without dispatching it.
func prepareMCPWalletSend(ctx context.Context,
	client wavewalletrpc.WalletServiceClient,
	args mcpSendPrepareArgs) (*mcp.CallToolResult, error) {

	offchain, err := parseDirectionField(args.Direction)
	if err != nil {
		return nil, err
	}
	req, err := buildWalletPrepareSendRequest(
		args.Destination, offchain, args.AmtSat, args.MaxFeeSat,
		args.Note, args.SweepAll,
	)
	if err != nil {
		return nil, err
	}

	resp, err := client.PrepareSend(ctx, req)
	if err != nil {
		return nil, mapWalletRPCError(err)
	}

	return mcpResult(resp)
}

// dispatchMCPWalletSend consumes a prepared intent without reconstructing or
// silently changing any payment parameter.
func dispatchMCPWalletSend(ctx context.Context,
	client wavewalletrpc.WalletServiceClient,
	args mcpSendArgs) (*mcp.CallToolResult, error) {

	if args.SendIntentID == "" {
		return nil, fmt.Errorf("send_intent_id is required; call " +
			"send.prepare first")
	}

	resp, err := client.Send(ctx, &wavewalletrpc.SendRequest{
		SendIntentId: args.SendIntentID,
	})
	if err != nil {
		return nil, mapWalletRPCError(err)
	}

	return mcpResult(resp)
}

// buildWalletActivityRequest translates MCP activityArgs into a ListRequest,
// applying the same activity filter parsing the CLI uses so the two
// surfaces stay in lockstep.
func buildWalletActivityRequest(pendingOnly bool, kinds []string, limit uint32,
	cursor string) (*wavewalletrpc.ListRequest, error) {

	req := &wavewalletrpc.ListRequest{
		View:        wavewalletrpc.ListView_LIST_VIEW_ACTIVITY,
		PendingOnly: pendingOnly,
		Limit:       limit,
		Cursor:      cursor,
	}

	for _, k := range kinds {
		parsed, err := parseEntryKind(k)
		if err != nil {
			return nil, err
		}
		req.Kinds = append(req.Kinds, parsed)
	}

	return req, nil
}

// buildWalletPrepareSendRequest assembles a PrepareSendRequest while running
// the
// CLI's input hardening (validateDestination, validateFreeText) and
// the offchain/onchain invariants so MCP can't construct a shape the
// CLI would reject.
func buildWalletPrepareSendRequest(dest string, offchain bool, amt,
	maxFee uint64, note string,
	sweepAll bool) (*wavewalletrpc.PrepareSendRequest, error) {

	if err := validateDestination(dest); err != nil {
		return nil, err
	}
	if err := validateFreeText("note", note); err != nil {
		return nil, err
	}

	if offchain && sweepAll {
		return nil, fmt.Errorf("sweep_all is only valid with onchain " +
			"sends (offchain=false)")
	}
	if !offchain {
		switch {
		case sweepAll && amt != 0:
			return nil, fmt.Errorf("sweep_all requires amt_sat=0")

		case !sweepAll && amt == 0:
			return nil, fmt.Errorf("amt_sat is required for " +
				"onchain sends (use sweep_all to drain)")
		}
	}

	req := &wavewalletrpc.PrepareSendRequest{
		AmtSat:    amt,
		MaxFeeSat: maxFee,
		Note:      note,
		SweepAll:  sweepAll,
	}
	if offchain {
		req.Destination = &wavewalletrpc.PrepareSendRequest_Invoice{
			Invoice: dest,
		}
	} else {
		req.Destination = &wavewalletrpc.
			PrepareSendRequest_OnchainAddress{
			OnchainAddress: dest,
		}
	}

	return req, nil
}

// parseDirectionField maps the MCP-friendly direction string onto the
// CLI's offchain bool. Empty defaults to offchain so an agent that
// just passes a destination keeps the safe invoice path. Unknown
// values are rejected explicitly rather than silently coerced.
func parseDirectionField(direction string) (bool, error) {
	switch direction {
	case "", "offchain":
		return true, nil

	case "onchain":
		return false, nil

	default:
		return false, fmt.Errorf("unknown direction %q (want "+
			"'offchain' or 'onchain')", direction)
	}
}

// mapWalletRPCError mirrors the CLI's withWalletClient mapping: a
// gRPC Unimplemented from a stub-build daemon becomes the actionable
// errWalletRPCDisabled instead of a generic status. Other errors pass
// through unchanged.
func mapWalletRPCError(err error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) == codes.Unimplemented {
		return errWalletRPCDisabled
	}

	return err
}
