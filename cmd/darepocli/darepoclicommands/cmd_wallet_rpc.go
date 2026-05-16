//go:build walletrpc

package darepoclicommands

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/spf13/cobra"
)

// addWalletRPCSubcommands attaches the simplified high-level wallet
// subcommands to the existing `wallet` parent. Each subcommand is a thin
// gRPC client wrapper: no key generation, no invoice parsing beyond a
// prefix sniff, no SQLite access, no resume calls. The daemon owns every
// behaviour those concerns map to.
func addWalletRPCSubcommands(parent *cobra.Command) {
	parent.AddCommand(
		newWalletSendCmd(), newWalletRecvCmd(), newWalletListCmd(),
		newWalletDepositCmd(), newWalletStatusCmd(),
	)
}

// invoicePrefixes are the human-readable BOLT-11 prefixes the CLI uses to
// route a Send argument to the invoice oneof rather than the onchain one.
// The daemon does the authoritative parse; the CLI just routes.
var invoicePrefixes = []string{"lnbc", "lntb", "lnbcrt", "lnsb"}

// newWalletSendCmd dispatches an outbound payment. The CLI does a cheap
// prefix sniff to decide between invoice and onchain; the daemon does the
// real parsing and returns InvalidArgument on mismatch.
func newWalletSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send <invoice-or-onchain-address>",
		Short: "Send a payment (LN invoice or onchain address)",
		Long: "Dispatches an outbound payment. BOLT-11 invoices route " +
			"through the swap subsystem; onchain destinations " +
			"route through cooperative leave. The daemon owns " +
			"all downstream lifecycle (no resume needed).",
		Args: cobra.ExactArgs(1),
		RunE: walletRPCSend,
	}
	cmd.Flags().Uint64("amt", 0,
		"amount in satoshis (required for onchain or amountless "+
			"invoice; must be 0 when --sweep-all is set)")
	cmd.Flags().Uint64("max_fee", 0,
		"max fee in satoshis; 0 lets the daemon use defaults")
	cmd.Flags().String("note", "",
		"caller-supplied label to attach to the entry")
	cmd.Flags().Bool("sweep-all", false,
		"onchain only: drain the wallet to the destination. "+
			"--amt MUST be 0 when set. Onchain sends sweep "+
			"WHOLE VTXOs so the destination receives the sum "+
			"of selected VTXOs (>= --amt). Inspect "+
			"actual_amount_sat on the response before treating "+
			"the send as confirmed.")

	return cmd
}

// newWalletRecvCmd opens a swap-in and returns a daemon-signed BOLT-11
// invoice the caller can hand out.
func newWalletRecvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recv",
		Short: "Receive a payment by generating a Lightning invoice",
		Long: "Asks the daemon to open a swap-in and return a " +
			"BOLT-11 invoice signed with a daemon-managed key. " +
			"The daemon waits for the payer and completes the " +
			"flow in the background.",
		RunE: walletRPCRecv,
	}
	cmd.Flags().Uint64("amt", 0, "amount in satoshis (required)")
	cmd.Flags().String("memo", "",
		"optional human-readable memo embedded in the invoice")

	return cmd
}

// newWalletListCmd returns the unified, normalized wallet history.
func newWalletListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List unified wallet history (send, recv, deposit, exit)",
		Long: "Returns the unified, normalized wallet history merged " +
			"across the swap subsystem and the daemon's ledger.",
		RunE: walletRPCList,
	}
	cmd.Flags().Bool("pending", false,
		"only show entries still in flight")
	cmd.Flags().StringSlice("kind", nil,
		"filter by kind (send,recv,deposit,exit); repeatable")
	cmd.Flags().Uint32("limit", 0,
		"page size; 0 uses the daemon default")
	cmd.Flags().Uint32("offset", 0,
		"pagination offset")

	return cmd
}

// newWalletDepositCmd returns a fresh boarding onchain address.
func newWalletDepositCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deposit",
		Short: "Get a fresh boarding onchain address for funding",
		Long: "Returns an onchain address the caller can fund. The " +
			"daemon rolls the boarding output into the next round.",
		RunE: walletRPCDeposit,
	}
	cmd.Flags().Uint64("amt_hint", 0,
		"optional expected deposit amount (for accounting)")

	return cmd
}

// newWalletStatusCmd composes daemon readiness, balance summary, and
// pending count into one snapshot.
func newWalletStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print wallet readiness, balance, and pending count",
		Long: "Combines GetInfo + Balance + pending-entry count into " +
			"one wallet-level status summary.",
		RunE: walletRPCStatus,
	}
}

// walletRPCSend is the RunE handler for `wallet send`.
func walletRPCSend(cmd *cobra.Command, args []string) error {
	dest := strings.TrimSpace(args[0])
	if dest == "" {
		return fmt.Errorf("destination is required")
	}
	amt, _ := cmd.Flags().GetUint64("amt")
	maxFee, _ := cmd.Flags().GetUint64("max_fee")
	note, _ := cmd.Flags().GetString("note")
	sweepAll, _ := cmd.Flags().GetBool("sweep-all")

	// Onchain-only: enforce the sweep_all / amt invariant up front so a
	// typo'd zero never lands on the wallet RPC. The wallet handler
	// re-checks defensively, but the CLI is the most common entry
	// point.
	isInvoice := isInvoicePrefix(dest)
	if !isInvoice {
		switch {
		case sweepAll && amt != 0:
			return fmt.Errorf("--sweep-all requires --amt=0 " +
				"(amt is implied by sweeping every live VTXO)")

		case !sweepAll && amt == 0:
			return fmt.Errorf("--amt is required for onchain " +
				"sends (use --sweep-all to drain the wallet)")
		}
	}

	req := &walletrpc.SendRequest{
		AmtSat:    amt,
		MaxFeeSat: maxFee,
		Note:      note,
		SweepAll:  sweepAll,
	}
	if isInvoice {
		req.Destination = &walletrpc.SendRequest_Invoice{
			Invoice: dest,
		}
	} else {
		req.Destination = &walletrpc.SendRequest_OnchainAddress{
			OnchainAddress: dest,
		}
	}

	return withWalletClient(
		cmd,
		func(c walletrpc.WalletServiceClient) error {
			resp, err := c.Send(cmd.Context(), req)
			if err != nil {
				return err
			}

			// For onchain sends actual_amount_sat may exceed
			// --amt under the v1 whole-VTXO sweep semantics.
			// Surface it on stderr so shell pipelines can still
			// consume the JSON body and a human reading the
			// terminal sees the real outflow.
			actual := resp.GetActualAmountSat()
			if !isInvoice && actual != int64(amt) {
				fmt.Fprintf(
					cmd.ErrOrStderr(),
					"note: actual_amount_sat=%d "+
						"exceeds --amt=%d due to "+
						"whole-VTXO sweep "+
						"semantics\n",
					actual, amt,
				)
			}

			return walletPrintJSON(resp)
		},
	)
}

// walletRPCRecv is the RunE handler for `wallet recv`.
func walletRPCRecv(cmd *cobra.Command, _ []string) error {
	amt, _ := cmd.Flags().GetUint64("amt")
	memo, _ := cmd.Flags().GetString("memo")
	if amt == 0 {
		return fmt.Errorf("--amt is required")
	}

	return withWalletClient(
		cmd,
		func(c walletrpc.WalletServiceClient) error {
			resp, err := c.Recv(
				cmd.Context(), &walletrpc.RecvRequest{
					AmtSat: amt,
					Memo:   memo,
				},
			)
			if err != nil {
				return err
			}

			return walletPrintJSON(resp)
		},
	)
}

// walletRPCList is the RunE handler for `wallet list`.
func walletRPCList(cmd *cobra.Command, _ []string) error {
	pending, _ := cmd.Flags().GetBool("pending")
	kinds, _ := cmd.Flags().GetStringSlice("kind")
	limit, _ := cmd.Flags().GetUint32("limit")
	offset, _ := cmd.Flags().GetUint32("offset")

	req := &walletrpc.ListRequest{
		PendingOnly: pending,
		Limit:       limit,
		Offset:      offset,
	}
	for _, k := range kinds {
		parsed, err := parseEntryKind(k)
		if err != nil {
			return err
		}
		req.Kinds = append(req.Kinds, parsed)
	}

	return withWalletClient(
		cmd,
		func(c walletrpc.WalletServiceClient) error {
			resp, err := c.List(cmd.Context(), req)
			if err != nil {
				return err
			}

			return walletPrintJSON(resp)
		},
	)
}

// walletRPCDeposit is the RunE handler for `wallet deposit`.
func walletRPCDeposit(cmd *cobra.Command, _ []string) error {
	amtHint, _ := cmd.Flags().GetUint64("amt_hint")

	return withWalletClient(
		cmd,
		func(c walletrpc.WalletServiceClient) error {
			resp, err := c.Deposit(
				cmd.Context(), &walletrpc.DepositRequest{
					AmtSatHint: amtHint,
				},
			)
			if err != nil {
				return err
			}

			return walletPrintJSON(resp)
		},
	)
}

// walletRPCStatus is the RunE handler for `wallet status`.
func walletRPCStatus(cmd *cobra.Command, _ []string) error {
	return withWalletClient(
		cmd,
		func(c walletrpc.WalletServiceClient) error {
			resp, err := c.Status(
				cmd.Context(), &walletrpc.StatusRequest{},
			)
			if err != nil {
				return err
			}

			return walletPrintJSON(resp)
		},
	)
}

// isInvoicePrefix reports whether s starts with a known BOLT-11 prefix.
// The check is intentionally cheap; the daemon does the authoritative
// parse.
func isInvoicePrefix(s string) bool {
	lower := strings.ToLower(s)
	for _, p := range invoicePrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}

	return false
}

// parseEntryKind maps a user-facing kind string to the proto enum.
func parseEntryKind(s string) (walletrpc.EntryKind, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "send":
		return walletrpc.EntryKind_ENTRY_KIND_SEND, nil

	case "recv", "receive":
		return walletrpc.EntryKind_ENTRY_KIND_RECV, nil

	case "deposit":
		return walletrpc.EntryKind_ENTRY_KIND_DEPOSIT, nil

	case "exit":
		return walletrpc.EntryKind_ENTRY_KIND_EXIT, nil

	default:
		return walletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
			fmt.Errorf("unknown kind %q (send|recv|deposit|exit)",
				s)
	}
}

// withWalletClient dials the daemon's WalletService and invokes fn with
// the resulting client. The transport reuses the existing getDaemonConn
// helper so the wallet subcommands honor the same global flags
// (--rpcserver, --tlscertpath, --no-tls) as every other darepocli verb.
func withWalletClient(cmd *cobra.Command,
	fn func(walletrpc.WalletServiceClient) error) error {

	conn, err := getDaemonConn(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	return fn(walletrpc.NewWalletServiceClient(conn))
}

// walletPrintJSON writes a proto message as pretty-printed JSON to
// stdout. The helper is local to the wallet-rpc CLI subcommands so it
// does not collide with similar helpers in other CLI files.
func walletPrintJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))

	return nil
}
