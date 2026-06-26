//go:build swapruntime

package darepoclicommands

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/spf13/cobra"
)

// errPaymentHashRequired is the shared message for swap show / resume
// when neither the positional arg nor the --json input supplies a
// payment_hash. Lifted out of the inline string concatenations so the
// llformat wrapping cannot split the token name across a line.
const errPaymentHashRequired = "payment_hash is required: pass the " +
	"positional argument or --json with payment_hash"

func newSwapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "swap",
		Short: "Lightning swap operations",
		Long: "Swap between Lightning and Ark via the daemon-owned " +
			"SwapClientService.",
	}

	cmd.AddCommand(
		newSwapListCmd(), newSwapShowCmd(), newSwapReceiveCmd(),
		newSwapPayCmd(), newSwapResumeCmd(), newSwapWatchCmd(),
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

			req := &swapclientrpc.ListSwapsRequest{}
			if err := parseRequest(cmd, req, func() error {
				pendingOnly, _ := cmd.Flags().GetBool("pending")
				req.PendingOnly = pendingOnly

				return nil
			}); err != nil {
				return err
			}

			// Use cmd.Context() so Ctrl+C / SIGTERM cancels the
			// RPC. context.Background() here would leave the
			// gRPC call running until the daemon responds.
			resp, err := client.ListSwaps(cmd.Context(), req)
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
// swap by payment hash. The positional arg is OPTIONAL at the cobra layer so
// an agent can drive the verb entirely through `--json`; the actual
// payment_hash requirement is enforced after parseRequest returns so it
// applies on both the positional and the --json path.
func newSwapShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show [payment_hash]",
		Short: "Show one persisted Lightning swap session",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, conn, err := getSwapClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			req := &swapclientrpc.GetSwapRequest{}
			if err := parseRequest(cmd, req, func() error {
				if len(args) != 1 {
					return PrintError(
						"INVALID_ARGS",
						errPaymentHashRequired,
					)
				}
				req.PaymentHash = args[0]

				return nil
			}); err != nil {
				return err
			}

			// Post-parse validation runs on both the flag and
			// --json paths so a malformed payment_hash slipping
			// in via --json is still caught client-side.
			if err := validatePaymentHash(
				req.PaymentHash,
			); err != nil {
				return PrintError("INVALID_ARGS", err.Error())
			}

			resp, err := client.GetSwap(cmd.Context(), req)
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

			req := &swapclientrpc.StartReceiveRequest{}
			if err := parseRequest(cmd, req, func() error {
				amount, _ := cmd.Flags().GetInt64("amount")
				req.AmountSat = amount

				return nil
			}); err != nil {
				return err
			}

			// Validate after parseRequest so the requirement
			// applies whether the amount came from --amount or
			// from --json.
			if req.AmountSat <= 0 {
				return PrintError(
					"INVALID_ARGS", "amount_sat must "+
						"be positive (pass "+
						"--amount or --json with a "+
						"positive amount_sat)",
				)
			}

			resp, err := client.StartReceive(cmd.Context(), req)
			if err != nil {
				return mapSwapRuntimeRPCError(err)
			}

			return printJSON(resp)
		},
	}

	// --amount is no longer cobra-required because that fires
	// before RunE and blocks the --json path. The post-parse check
	// above does the equivalent validation on both surfaces.
	cmd.Flags().Int64("amount", 0,
		"amount in satoshis to receive (required; or pass via --json)")

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

			req := &swapclientrpc.StartPayRequest{}
			if err := parseRequest(cmd, req, func() error {
				invoice, _ := cmd.Flags().GetString("invoice")
				maxFee, _ := cmd.Flags().GetUint64("maxfee")
				req.Invoice = invoice
				req.MaxFeeSat = maxFee

				return nil
			}); err != nil {
				return err
			}

			// Validate invoice on both flag and --json paths so
			// the agent-cli input-hardening contract holds even
			// when an agent submits the request as raw JSON.
			if err := validateInvoice(req.Invoice); err != nil {
				return PrintError("INVALID_ARGS", err.Error())
			}

			resp, err := client.StartPay(cmd.Context(), req)
			if err != nil {
				return mapSwapRuntimeRPCError(err)
			}

			return printJSON(resp)
		},
	}

	// --invoice is no longer cobra-required to keep the --json path
	// reachable; validateInvoice runs post-parse on both surfaces.
	cmd.Flags().String("invoice", "",
		"BOLT-11 Lightning invoice to pay (required; or via --json)")
	cmd.Flags().Uint64("maxfee", 0,
		"maximum swap fee in satoshis (0 lets the daemon default the "+
			"cap to ~1% of the amount, with a small floor)")

	return cmd
}

// newSwapResumeCmd builds the manual wake-up command for persisted swaps. The
// daemon still deduplicates by payment hash, so this command cannot create a
// second in-process FSM driver for an already active swap.
func newSwapResumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume [payment_hash]",
		Short: "Resume a persisted Lightning swap session",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, conn, err := getSwapClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			req := &swapclientrpc.ResumeSwapRequest{}
			if err := parseRequest(cmd, req, func() error {
				if len(args) != 1 {
					return PrintError(
						"INVALID_ARGS",
						errPaymentHashRequired,
					)
				}
				req.PaymentHash = args[0]

				direction, _ := cmd.Flags().GetString(
					"direction",
				)
				rpcDirection, err := parseSwapRPCDirection(
					direction,
				)
				if err != nil {
					return PrintError(
						"INVALID_ARGS", err.Error(),
					)
				}
				req.Direction = rpcDirection

				return nil
			}); err != nil {
				return err
			}

			// Post-parse validation: payment_hash and direction
			// are required on both the flag and --json paths.
			if err := validatePaymentHash(
				req.PaymentHash,
			); err != nil {
				return PrintError("INVALID_ARGS", err.Error())
			}
			if req.Direction ==
				swapclientrpc.SwapDirection_SWAP_DIRECTION_UNSPECIFIED {
				return PrintError(
					"INVALID_ARGS", "direction is "+
						"required (pay or receive)",
				)
			}

			resp, err := client.ResumeSwap(cmd.Context(), req)
			if err != nil {
				return mapSwapRuntimeRPCError(err)
			}

			return printJSON(resp)
		},
	}

	// --direction is no longer cobra-required; post-parse validation
	// enforces it on both the flag and --json paths.
	cmd.Flags().String("direction", "",
		"swap direction to resume: pay or receive "+
			"(required; or via --json)")

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

			req := &swapclientrpc.SubscribeSwapsRequest{}
			if err := parseRequest(cmd, req, func() error {
				pendingOnly, _ := cmd.Flags().GetBool("pending")
				includeExisting, _ := cmd.Flags().GetBool(
					"include-existing",
				)
				req.PendingOnly = pendingOnly
				req.IncludeExisting = includeExisting

				return nil
			}); err != nil {
				return err
			}

			stream, err := client.SubscribeSwaps(
				cmd.Context(), req,
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
func getSwapClient(cmd *cobra.Command) (swapclientrpc.SwapClientServiceClient,
	interface{ Close() error }, error) {

	conn, err := getDaemonConn(cmd)
	if err != nil {
		return nil, nil, err
	}

	return swapclientrpc.NewSwapClientServiceClient(conn), conn, nil
}

// parseSwapRPCDirection maps the CLI's human-facing direction flag into the
// daemon RPC enum. A missing or unknown direction is rejected before the resume
// request reaches the daemon.
func parseSwapRPCDirection(direction string) (swapclientrpc.SwapDirection,
	error) {

	switch strings.ToLower(direction) {
	case "":
		return swapclientrpc.
			SwapDirection_SWAP_DIRECTION_UNSPECIFIED, nil

	case "pay":
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY, nil

	case "receive":
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE, nil

	default:
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_UNSPECIFIED,
			fmt.Errorf("unknown swap direction %q", direction)
	}
}
