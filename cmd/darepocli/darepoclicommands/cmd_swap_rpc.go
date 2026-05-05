//go:build swapruntime

package darepoclicommands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newSwapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "swap",
		Short: "Lightning swap operations",
		Long: "Swap between Lightning and Ark via the daemon-owned " +
			"SwapClientService.",
	}

	cmd.AddCommand(
		newSwapListCmd(),
		newSwapShowCmd(),
		newSwapReceiveCmd(),
		newSwapPayCmd(),
		newSwapResumeCmd(),
		newSwapWatchCmd(),
	)

	return cmd
}

// newSwapListCmd builds the daemon-backed command that reads persisted swap
// summaries from SwapClientService. The CLI is intentionally a thin RPC
// renderer here so swap progress continues in darepod after the command exits.
func newSwapListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List persisted Lightning swap sessions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, conn, err := getSwapClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			pendingOnly, _ := cmd.Flags().GetBool("pending")

			resp, err := client.ListSwaps(
				context.Background(),
				&swapclientrpc.ListSwapsRequest{
					PendingOnly: pendingOnly,
				},
			)
			if err != nil {
				return mapSwapRuntimeRPCError(err)
			}

			return printJSON(resp)
		},
	}

	cmd.Flags().Bool("pending", false,
		"show only non-terminal resumable swaps")

	return cmd
}

// newSwapShowCmd builds the daemon-backed command that fetches one persisted
// swap by payment hash. This exercises the GetSwap RPC directly instead of
// teaching the CLI how to inspect the daemon-owned swap database.
func newSwapShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show [payment_hash]",
		Short: "Show one persisted Lightning swap session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, conn, err := getSwapClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			resp, err := client.GetSwap(
				context.Background(),
				&swapclientrpc.GetSwapRequest{
					PaymentHash: args[0],
				},
			)
			if err != nil {
				return mapSwapRuntimeRPCError(err)
			}

			return printJSON(resp)
		},
	}

	return cmd
}

// newSwapReceiveCmd builds the daemon-backed command that creates a receive
// swap intent. The command returns after the daemon has durably persisted the
// invoice session and taken ownership of the background receive worker.
func newSwapReceiveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "receive",
		Short: "Receive BTC via Lightning into Ark",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, conn, err := getSwapClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			amount, _ := cmd.Flags().GetInt64("amount")

			resp, err := client.StartReceive(
				context.Background(),
				&swapclientrpc.StartReceiveRequest{
					AmountSat: amount,
				},
			)
			if err != nil {
				return mapSwapRuntimeRPCError(err)
			}

			return printJSON(resp)
		},
	}

	cmd.Flags().Int64("amount", 0,
		"amount in satoshis to receive (required)")
	_ = cmd.MarkFlagRequired("amount")

	return cmd
}

// newSwapPayCmd builds the daemon-backed command that creates a pay swap
// intent. Once the RPC returns, the daemon owns funding, preimage observation,
// refund, and terminalization work for that payment hash.
func newSwapPayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pay",
		Short: "Pay a Lightning invoice from Ark funds",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, conn, err := getSwapClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			invoice, _ := cmd.Flags().GetString("invoice")
			maxFee, _ := cmd.Flags().GetUint64("maxfee")

			resp, err := client.StartPay(
				context.Background(),
				&swapclientrpc.StartPayRequest{
					Invoice:   invoice,
					MaxFeeSat: maxFee,
				},
			)
			if err != nil {
				return mapSwapRuntimeRPCError(err)
			}

			return printJSON(resp)
		},
	}

	cmd.Flags().String("invoice", "",
		"BOLT-11 Lightning invoice to pay (required)")
	_ = cmd.MarkFlagRequired("invoice")
	cmd.Flags().Uint64("maxfee", 0,
		"maximum fee in satoshis (0 = no limit)")

	return cmd
}

// newSwapResumeCmd builds the manual wake-up command for persisted swaps. The
// daemon still deduplicates by payment hash, so this command cannot create a
// second in-process FSM driver for an already active swap.
func newSwapResumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume [payment_hash]",
		Short: "Resume a persisted Lightning swap session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, conn, err := getSwapClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			direction, _ := cmd.Flags().GetString("direction")
			rpcDirection, err := parseSwapRPCDirection(direction)
			if err != nil {
				return err
			}

			resp, err := client.ResumeSwap(
				context.Background(),
				&swapclientrpc.ResumeSwapRequest{
					PaymentHash: args[0],
					Direction:   rpcDirection,
				},
			)
			if err != nil {
				return mapSwapRuntimeRPCError(err)
			}

			return printJSON(resp)
		},
	}

	cmd.Flags().String("direction", "",
		"swap direction to resume: pay or receive (required)")
	_ = cmd.MarkFlagRequired("direction")

	return cmd
}

// newSwapWatchCmd builds the streaming observer command for daemon-owned swap
// summaries. It may emit existing rows first, then live worker-exit updates as
// reported by SwapClientService.
func newSwapWatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Stream swap updates",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, conn, err := getSwapClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			pendingOnly, _ := cmd.Flags().GetBool("pending")
			includeExisting, _ := cmd.Flags().GetBool(
				"include-existing",
			)

			stream, err := client.SubscribeSwaps(
				context.Background(),
				&swapclientrpc.SubscribeSwapsRequest{
					IncludeExisting: includeExisting,
					PendingOnly:     pendingOnly,
				},
			)
			if err != nil {
				return mapSwapRuntimeRPCError(err)
			}

			for {
				resp, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return mapSwapRuntimeRPCError(err)
				}
				if err := printJSON(resp); err != nil {
					return err
				}
			}
		},
	}

	cmd.Flags().Bool("pending", false,
		"stream only pending swap updates")
	cmd.Flags().Bool("include-existing", true,
		"emit existing swaps before live updates")

	return cmd
}

// getSwapClient constructs a SwapClientService client over the normal daemon
// gRPC connection. Keeping this helper in the CLI package lets all swap
// commands share TLS and endpoint handling with the rest of darepocli.
func getSwapClient(
	cmd *cobra.Command) (swapclientrpc.SwapClientServiceClient,
	interface{ Close() error }, error) {

	conn, err := getDaemonConn(cmd)
	if err != nil {
		return nil, nil, err
	}

	return swapclientrpc.NewSwapClientServiceClient(conn), conn, nil
}

// mapSwapRuntimeRPCError turns the gRPC unknown-service response from a default
// daemon into the same user-facing guidance as the default CLI stub. Other
// service errors are returned unchanged so daemon-side validation and terminal
// swap errors remain visible.
func mapSwapRuntimeRPCError(err error) error {
	if err == nil {
		return nil
	}

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unimplemented {
		return err
	}

	msg := st.Message()
	if !strings.Contains(msg, "SwapClientService") &&
		!strings.Contains(msg, "swapclientrpc") {

		return err
	}

	return fmt.Errorf(
		"daemon was built without swapruntime support; " +
			"rebuild darepod with tags=\"swapruntime\"",
	)
}

// parseSwapRPCDirection maps the CLI's human-facing direction flag into the
// daemon RPC enum. A missing or unknown direction is rejected before the resume
// request reaches the daemon.
func parseSwapRPCDirection(
	direction string) (swapclientrpc.SwapDirection, error) {

	switch strings.ToLower(direction) {
	case "pay":
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY, nil

	case "receive":
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE, nil

	default:
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_UNSPECIFIED,
			fmt.Errorf("unknown swap direction %q", direction)
	}
}
