package darepoclicommands

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
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
			"  darepocli activity\n" +
			"  darepocli activity --pending --kind send,recv\n" +
			"  darepocli activity --format json\n" +
			"  darepocli activity inspect <id>",
		Args: cobra.NoArgs,
		RunE: walletActivity,
	}

	cmd.Flags().Bool("pending", false,
		"only show entries still in flight")
	cmd.Flags().StringSlice("kind", nil,
		"filter by kind (send,recv,deposit,exit); repeatable")
	cmd.Flags().Uint32("limit", 0,
		"page size; 0 uses the daemon default")
	cmd.Flags().Uint32("offset", 0, "pagination offset")
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
	offset, _ := cmd.Flags().GetUint32("offset")
	format, _ := cmd.Flags().GetString("format")

	if err := validateListFormat(
		format, walletrpc.ListView_LIST_VIEW_ACTIVITY,
	); err != nil {
		return err
	}

	req := &walletrpc.ListRequest{
		View:        walletrpc.ListView_LIST_VIEW_ACTIVITY,
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
		cmd, func(c walletrpc.WalletServiceClient) error {
			resp, err := c.List(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("activity: %w", err)
			}

			switch format {
			case "", "table":
				return printWalletActivityTable(resp)

			case "expanded", "x":
				return printWalletActivityExpanded(resp)

			case "json":
				return printWalletProto(resp)
			}

			return nil
		},
	)
}
