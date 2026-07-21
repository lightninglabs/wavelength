package waveclicommands

import (
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/spf13/cobra"
)

// newActivityCmd builds the top-level `activity` verb. It returns the
// wallet's user-facing activity feed; deeper technical traces live under
// `activity inspect`.
func newActivityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "activity",
		Short: "Show wallet activity",
		Long: "Show merged wallet activity: sends, receives, " +
			"deposits, and exits.\n\n" +
			"Examples:\n" +
			"  wavecli activity\n" +
			"  wavecli activity --pending --kind send,recv\n" +
			"  wavecli activity --format json\n" +
			"  wavecli activity --cursor <next_cursor>\n" +
			"  wavecli activity inspect <id>",
		Args: cobra.NoArgs,
		RunE: walletActivity,
	}

	cmd.Flags().Bool("pending", false,
		"only show entries still in flight")
	cmd.Flags().StringSlice("kind", nil,
		"filter by kind (send,recv,deposit,exit); repeatable")
	cmd.Flags().Uint32("limit", 0,
		"page size; 0 uses the daemon default")
	cmd.Flags().String("cursor", "",
		"activity page token from a prior page's next_cursor")
	cmd.Flags().String("format", "table",
		"output format (table|expanded|x|json)")

	cmd.AddCommand(newActivityInspectCmd())

	return cmd
}

// walletActivity implements the top-level `activity` verb.
func walletActivity(cmd *cobra.Command, _ []string) error {
	pending, _ := cmd.Flags().GetBool("pending")
	kinds, _ := cmd.Flags().GetStringSlice("kind")
	limit, _ := cmd.Flags().GetUint32("limit")
	cursor, _ := cmd.Flags().GetString("cursor")
	format, _ := cmd.Flags().GetString("format")

	if err := validateListFormat(
		format, wavewalletrpc.ListView_LIST_VIEW_ACTIVITY,
	); err != nil {
		return err
	}

	req := &wavewalletrpc.ListRequest{
		View:        wavewalletrpc.ListView_LIST_VIEW_ACTIVITY,
		PendingOnly: pending,
		Limit:       limit,
		Cursor:      cursor,
	}
	for _, k := range kinds {
		parsed, err := parseEntryKind(k)
		if err != nil {
			return err
		}
		req.Kinds = append(req.Kinds, parsed)
	}

	return withWalletClient(
		cmd, func(c wavewalletrpc.WalletServiceClient) error {
			ctx, cancel := rpcContext(cmd)
			defer cancel()

			resp, err := c.List(ctx, req)
			if err != nil {
				return fmt.Errorf("activity: %w", err)
			}

			switch format {
			case "", "table":
				if err := printWalletActivityTable(
					resp,
				); err != nil {
					return err
				}

				return printWalletActivityNextPage(resp)

			case "expanded", "x":
				if err := printWalletActivityExpanded(
					resp,
				); err != nil {
					return err
				}

				return printWalletActivityNextPage(resp)

			case "json":
				return printWalletProto(resp)
			}

			return nil
		},
	)
}
