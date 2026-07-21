package waveclicommands

import (
	"fmt"
	"os"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
)

// newFeesCmd creates the fees subcommand group.
func newFeesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fees",
		Short: "Fee estimation and history",
		Long: "Commands for estimating Ark round fees and " +
			"viewing fee history from the local ledger.",
	}

	cmd.AddCommand(
		newFeesEstimateCmd(),
		newFeesHistoryCmd(),
	)

	return cmd
}

// newFeesEstimateCmd creates the "fees estimate" subcommand.
func newFeesEstimateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "estimate",
		Short: "Estimate fee for a VTXO operation",
		Long: "Returns an itemized fee estimate for a " +
			"given amount at the operator's current " +
			"rates and treasury utilization. The value " +
			"is advisory: the binding per-round fee is " +
			"set by the server-issued JoinRoundQuote at " +
			"seal time and may differ from this estimate. " +
			"Shows liquidity fee, on-chain share, margin, " +
			"total fee, effective rate, and minimum " +
			"viable VTXO.\n\n" +
			"To preview the fee for refreshing specific " +
			"VTXOs without looking up their amounts and " +
			"remaining lifetimes by hand, use `ark vtxos " +
			"refresh --dry_run` instead: it resolves each " +
			"selected VTXO and returns a per-outpoint " +
			"estimate.",
		RunE: feesEstimate,
	}

	f := cmd.Flags()
	f.Int64("amount", 0,
		"VTXO amount in satoshis to estimate fees for")

	// The default is true (boarding) because that is the
	// expected first-time CLI use. The corresponding proto
	// field defaults to false (the zero value), so callers
	// constructing the request programmatically see refresh
	// semantics by default -- the divergence is intentional
	// and matches what a user holding just the CLI help text
	// expects when they type `fees estimate --amount=N`.
	f.Bool(
		"boarding", true, "estimate for boarding (true, default) "+
			"or refresh (false); CLI default diverges from the "+
			"proto zero value to match first-time use",
	)
	f.Uint32(
		"remaining-blocks", 0, "remaining VTXO lifetime in blocks "+
			"(refresh only, required when --boarding=false)",
	)

	return cmd
}

// feesEstimate executes the EstimateFee RPC.
func feesEstimate(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &waverpc.EstimateFeeRequest{}
	if err := parseRequest(cmd, req, func() error {
		amount, _ := cmd.Flags().GetInt64("amount")
		if amount <= 0 {
			return fmt.Errorf("--amount is required and must be " +
				"positive")
		}

		boarding, _ := cmd.Flags().GetBool("boarding")
		remaining, _ := cmd.Flags().GetUint32(
			"remaining-blocks",
		)

		// Refresh estimates need a non-zero remaining-blocks
		// figure to compute the time-value-of-money component;
		// the operator rejects the request otherwise. Fail
		// client-side so the user gets a clear flag-level
		// error instead of an opaque RPC failure.
		if !boarding && remaining == 0 {
			return fmt.Errorf("--remaining-blocks is required " +
				"when --boarding=false")
		}

		req.AmountSat = amount
		req.IsBoarding = boarding
		req.RemainingBlocks = remaining

		return nil
	}); err != nil {
		return err
	}

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	resp, err := client.EstimateFee(ctx, req)
	if err != nil {
		return fmt.Errorf("EstimateFee RPC failed: %w", err)
	}

	// below_dust_warning is advisory on the wire: the operator
	// will happily estimate a fee for a dust-level VTXO, but the
	// resulting VTXO would be uneconomic to produce. Surface the
	// warning on stderr so an interactive user sees it alongside
	// the JSON output and an automated caller that ignores the
	// field still pays attention to the exit stream.
	if resp != nil && resp.BelowDustWarning {
		fmt.Fprintf(
			os.Stderr, "warning: requested amount %d is below "+
				"the operator's minimum viable VTXO (%d "+
				"sat). Producing this VTXO will cost more "+
				"to exit than it holds -- consider raising "+
				"--amount above the minimum.\n", req.AmountSat,
			resp.MinViableAmountSat,
		)
	}

	return printJSON(resp)
}

// newFeesHistoryCmd creates the "fees history" subcommand.
func newFeesHistoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show fee payment history",
		Long: "Returns paginated fee ledger entries from " +
			"the client's local accounting database, " +
			"including total fees paid.",
		RunE: feesHistory,
	}

	f := cmd.Flags()
	f.Uint32("limit", 50, "maximum number of entries")
	f.Uint32("offset", 0, "number of entries to skip")

	return cmd
}

// feesHistory executes the GetFeeHistory RPC.
func feesHistory(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &waverpc.GetFeeHistoryRequest{}
	if err := parseRequest(cmd, req, func() error {
		limit, _ := cmd.Flags().GetUint32("limit")
		offset, _ := cmd.Flags().GetUint32("offset")

		req.Limit = limit
		req.Offset = offset

		return nil
	}); err != nil {
		return err
	}

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	resp, err := client.GetFeeHistory(ctx, req)
	if err != nil {
		return fmt.Errorf("GetFeeHistory RPC failed: %w", err)
	}

	return printJSON(resp)
}
