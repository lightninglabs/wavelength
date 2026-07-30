package waveclicommands

import (
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

// newArkCmd builds the `ark` parent that hosts the advanced low-level
// commands targeting raw waverpc methods: VTXO inventory and
// lifecycle, round state, OOR sessions, boarding, sweeps, fees, raw
// ledger transactions, and the raw in-round / OOR send paths.
//
// The everyday 7-verb surface (create / unlock / send / recv / list /
// balance / exit) lives at the top level; this parent is the
// power-user entry point.
func newArkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ark",
		Short: "Advanced Ark protocol commands (raw waverpc)",
		Long: "Power-user subcommands that surface the raw waverpc " +
			"methods underlying the everyday wallet verbs. " +
			"Useful for inspection, debugging, and operator " +
			"runbooks.",
	}

	cmd.AddCommand(
		newVTXOsCmd(), newRoundsCmd(), newOORCmd(), newBoardCmd(),
		newSweepCmd(), newFeesCmd(), newArkListTransactionsCmd(),
		newArkSendCmd(),
	)

	return cmd
}

const arkTransactionDateLayout = "2006-01-02"

// newArkListTransactionsCmd builds the `ark listtransactions` advanced
// subcommand. It is the raw waverpc.ListTransactions surface; the
// everyday wallet activity path composes the same data with the
// internal correlators stripped.
func newArkListTransactionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "listtransactions",
		Short: "Raw paginated transaction history (advanced)",
		Long: "Returns the daemon's unfiltered local transaction " +
			"history (ledger + boarding sweeps) with full " +
			"internal correlators. For the wallet-shaped view " +
			"use `wavecli activity`.",
		RunE: listTransactions,
	}

	f := cmd.Flags()
	f.String(
		"from", "",
		"only include entries at or after this ISO 8601 time",
	)
	f.String(
		"to", "",
		"only include entries at or before this ISO 8601 time",
	)
	f.Uint32("limit", 50, "maximum number of entries")
	f.Uint32("offset", 0, "number of filtered entries to skip")
	f.String(
		"type", "",
		"optional type filter: boarding, round, oor, or sweep",
	)
	addListOutputFlags(cmd, "transaction")

	return cmd
}

// listTransactions executes the waverpc.ListTransactions RPC.
func listTransactions(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &waverpc.ListTransactionsRequest{}
	if err := parseRequest(cmd, req, func() error {
		from, _ := cmd.Flags().GetString("from")
		to, _ := cmd.Flags().GetString("to")
		limit, _ := cmd.Flags().GetUint32("limit")
		offset, _ := cmd.Flags().GetUint32("offset")
		filter, _ := cmd.Flags().GetString("type")

		fromUnix, err := parseTransactionTimeFlag("--from", from)
		if err != nil {
			return err
		}
		toUnix, err := parseTransactionTimeFlag("--to", to)
		if err != nil {
			return err
		}
		err = validateTransactionTimeWindow(fromUnix, toUnix)
		if err != nil {
			return err
		}
		if !transactionTypeFlagValid(filter) {
			return fmt.Errorf("--type must be one of boarding, " +
				"round, oor, or sweep")
		}

		req.FromUnixS = fromUnix
		req.ToUnixS = toUnix
		req.Limit = limit
		req.Offset = offset
		req.Type = filter

		return nil
	}); err != nil {
		return err
	}

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	resp, err := client.ListTransactions(ctx, req)
	if err != nil {
		return fmt.Errorf("ListTransactions RPC failed: %w", err)
	}

	items := make([]proto.Message, len(resp.Transactions))
	for i, t := range resp.Transactions {
		items[i] = t
	}

	return renderListOutput(cmd, resp, items)
}

// parseTransactionTimeFlag parses an ISO 8601 timestamp or date-only
// value into Unix seconds. Empty values leave the corresponding RPC
// bound open. For date-only values on --to the endpoint is widened to
// 23:59:59 so an inclusive day bound covers the whole day.
func parseTransactionTimeFlag(name, value string) (int64, error) {
	if value == "" {
		return 0, nil
	}

	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.Unix(), nil
	}

	if t, err := time.Parse(arkTransactionDateLayout, value); err == nil {
		if name == "--to" {
			endOfDay := t.Add(24*time.Hour - time.Second)

			return endOfDay.Unix(), nil
		}

		return t.Unix(), nil
	}

	return 0, fmt.Errorf("%s must be an ISO 8601 timestamp (for example "+
		"2026-05-08T00:00:00Z) or date (for example 2026-05-08)", name)
}

// validateTransactionTimeWindow rejects from > to so the daemon never
// sees an inverted window.
func validateTransactionTimeWindow(fromUnix, toUnix int64) error {
	if fromUnix != 0 && toUnix != 0 && fromUnix > toUnix {
		return fmt.Errorf("--from must be before or equal to --to")
	}

	return nil
}

// transactionTypeFlagValid reports whether the --type filter is one of
// the daemon's accepted values.
func transactionTypeFlagValid(s string) bool {
	switch s {
	case "", "boarding", "round", "oor", "sweep":
		return true

	default:
		return false
	}
}
