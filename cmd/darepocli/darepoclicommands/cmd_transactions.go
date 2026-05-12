package darepoclicommands

import (
	"fmt"
	"time"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
)

const transactionDateLayout = "2006-01-02"

// newListTransactionsCmd creates the top-level listtransactions command.
func newListTransactionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "listtransactions",
		Short: "List local transaction history",
		Long: "Returns a paginated newest-first transaction " +
			"history from the daemon's local accounting " +
			"database, with optional date and type filters.",
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

	return cmd
}

// listTransactions executes the ListTransactions RPC.
func listTransactions(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &daemonrpc.ListTransactionsRequest{}
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

	resp, err := client.ListTransactions(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("ListTransactions RPC failed: %w", err)
	}

	return printJSON(resp)
}

// parseTransactionTimeFlag parses an ISO 8601 timestamp or date-only value
// into Unix seconds. Empty values leave the corresponding RPC bound open.
func parseTransactionTimeFlag(name, value string) (int64, error) {
	if value == "" {
		return 0, nil
	}

	parsed, err := time.Parse(time.RFC3339, value)
	if err == nil {
		return parsed.Unix(), nil
	}

	parsed, dateErr := time.Parse(transactionDateLayout, value)
	if dateErr == nil {
		if name == "--to" {
			endOfDay := parsed.Add(24*time.Hour - time.Second)

			return endOfDay.Unix(), nil
		}

		return parsed.Unix(), nil
	}

	return 0, fmt.Errorf("%s must be an ISO 8601 timestamp (for example "+
		"2026-05-08T00:00:00Z) or date (for example 2026-05-08)", name)
}

// transactionTypeFlagValid returns true for the public transaction type
// filters accepted by the listtransactions command.
func transactionTypeFlagValid(filter string) bool {
	switch filter {
	case "", "boarding", "round", "oor", "sweep":
		return true

	default:
		return false
	}
}

// validateTransactionTimeWindow returns an error when both bounds are set and
// the lower bound is after the upper bound.
func validateTransactionTimeWindow(fromUnix, toUnix int64) error {
	if fromUnix != 0 && toUnix != 0 && fromUnix > toUnix {
		return fmt.Errorf("--from must be before or equal to --to")
	}

	return nil
}
