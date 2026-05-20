//go:build swapruntime

package darepoclicommands

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerMCPSwapTools registers the `swap.*` advanced surface as
// MCP tools. The streaming `swap.watch` verb is intentionally NOT
// surfaced over MCP: an MCP tool call is request/response, and the
// stream's natural consumer is a long-running CLI process. Agents
// that need streaming behavior should subscribe via the CLI directly.
//
// Input hardening (payment_hash shape, invoice trim) is shared with
// the CLI path through validatePaymentHash / validateInvoice so the
// two surfaces cannot drift on what shapes the daemon ever sees.
//
// mapSwapRuntimeRPCError mirrors the CLI's behavior of turning the
// daemon's gRPC Unimplemented for SwapClientService into actionable
// "rebuild with swapruntime" text.
//
// The client is taken as a typed interface (matching
// registerMCPWalletTools) so unit tests can inject a mock instead of
// a real gRPC connection.
func registerMCPSwapTools(s *mcp.Server,
	client swapclientrpc.SwapClientServiceClient) {

	registerMCPSwapReadTools(s, client)
	registerMCPSwapMutateTools(s, client)
}

// registerMCPSwapReadTools registers the read-only swap verbs (list,
// show).
func registerMCPSwapReadTools(s *mcp.Server,
	client swapclientrpc.SwapClientServiceClient) {

	type listArgs struct {
		PendingOnly bool `json:"pending_only,omitempty" jsonschema:"only return non-terminal resumable swaps"` //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "swap.list",
		Description: "List persisted Lightning swap sessions",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listArgs) (
		*mcp.CallToolResult, any, error) {

		resp, err := client.ListSwaps(
			ctx, &swapclientrpc.ListSwapsRequest{
				PendingOnly: args.PendingOnly,
			},
		)
		if err != nil {
			return nil, nil, mapSwapRuntimeRPCError(err)
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	type showArgs struct {
		PaymentHash string `json:"payment_hash" jsonschema:"32-byte payment hash (64 hex chars)"` //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "swap.show",
		Description: "Show one persisted Lightning swap session " +
			"by payment hash",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args showArgs) (
		*mcp.CallToolResult, any, error) {

		if err := validatePaymentHash(args.PaymentHash); err != nil {
			return nil, nil, err
		}
		resp, err := client.GetSwap(
			ctx, &swapclientrpc.GetSwapRequest{
				PaymentHash: args.PaymentHash,
			},
		)
		if err != nil {
			return nil, nil, mapSwapRuntimeRPCError(err)
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})
}

// registerMCPSwapMutateTools registers the mutating swap verbs
// (receive, pay, resume). Each runs the CLI's input validators so an
// MCP-driven swap cannot reach the daemon with a shape the CLI would
// reject.
func registerMCPSwapMutateTools(s *mcp.Server,
	client swapclientrpc.SwapClientServiceClient) {

	type receiveArgs struct {
		AmountSat int64 `json:"amount_sat" jsonschema:"amount in satoshis to receive (required)"` //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "swap.receive",
		Description: "Receive BTC via Lightning into Ark",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args receiveArgs) (
		*mcp.CallToolResult, any, error) {

		if args.AmountSat <= 0 {
			return nil, nil, fmt.Errorf("amount_sat must be " +
				"positive")
		}
		resp, err := client.StartReceive(
			ctx, &swapclientrpc.StartReceiveRequest{
				AmountSat: args.AmountSat,
			},
		)
		if err != nil {
			return nil, nil, mapSwapRuntimeRPCError(err)
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	type payArgs struct {
		Invoice   string `json:"invoice" jsonschema:"BOLT-11 invoice to pay (required)"`                          //nolint:ll
		MaxFeeSat uint64 `json:"max_fee_sat,omitempty" jsonschema:"maximum fee in satoshis; zero means no limit"` //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "swap.pay",
		Description: "Pay a Lightning invoice from Ark funds",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args payArgs) (
		*mcp.CallToolResult, any, error) {

		if err := validateInvoice(args.Invoice); err != nil {
			return nil, nil, err
		}
		resp, err := client.StartPay(
			ctx, &swapclientrpc.StartPayRequest{
				Invoice:   args.Invoice,
				MaxFeeSat: args.MaxFeeSat,
			},
		)
		if err != nil {
			return nil, nil, mapSwapRuntimeRPCError(err)
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})

	type resumeArgs struct {
		PaymentHash string `json:"payment_hash" jsonschema:"32-byte payment hash (64 hex chars)"`       //nolint:ll
		Direction   string `json:"direction" jsonschema:"swap direction to resume: 'pay' or 'receive'"` //nolint:ll
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "swap.resume",
		Description: "Resume a persisted Lightning swap session",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args resumeArgs) (
		*mcp.CallToolResult, any, error) {

		if err := validatePaymentHash(args.PaymentHash); err != nil {
			return nil, nil, err
		}
		direction, err := parseSwapRPCDirection(args.Direction)
		if err != nil {
			return nil, nil, err
		}
		resp, err := client.ResumeSwap(
			ctx, &swapclientrpc.ResumeSwapRequest{
				PaymentHash: args.PaymentHash,
				Direction:   direction,
			},
		)
		if err != nil {
			return nil, nil, mapSwapRuntimeRPCError(err)
		}

		r, err := mcpResult(resp)

		return r, nil, err
	})
}
