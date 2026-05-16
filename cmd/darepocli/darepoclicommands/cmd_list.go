package darepoclicommands

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/spf13/cobra"
)

// newListCmd builds the top-level `list` verb. The view enum selects
// which slice of wallet state to return; --pending and --kind apply
// only within the activity view. The response shape is a oneof body
// per view (activity / vtxos / onchain) so agents see a discriminated
// union rather than a polymorphic blob.
func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List wallet activity, VTXOs, or onchain history",
		Long: "Returns the unified wallet view selected by --view:\n" +
			"  activity (default) — merged send / recv / deposit " +
			"/ exit history\n" +
			"  vtxos    — live VTXO inventory\n" +
			"  onchain  — boarding deposits, sweeps, leave " +
			"outputs\n\n" +
			"--pending and --kind apply within the activity " +
			"view and are ignored for vtxos/onchain.\n\n" +
			"Examples:\n" +
			"  darepocli list                  " +
			"# activity (default)\n" +
			"  darepocli list --view vtxos\n" +
			"  darepocli list --view onchain --limit 100\n" +
			"  darepocli list --pending --kind send,recv",
		RunE: walletList,
	}

	cmd.Flags().String("view", "activity",
		"which slice of wallet state to return "+
			"(activity|vtxos|onchain)")
	cmd.Flags().Bool("pending", false,
		"only show entries still in flight (activity view only)")
	cmd.Flags().StringSlice("kind", nil,
		"filter activity by kind (send,recv,deposit,exit); "+
			"repeatable")
	cmd.Flags().Uint32("limit", 0,
		"page size; 0 uses the daemon default")
	cmd.Flags().Uint32("offset", 0, "pagination offset")

	return cmd
}

// walletList implements the top-level `list` verb.
func walletList(cmd *cobra.Command, _ []string) error {
	viewStr, _ := cmd.Flags().GetString("view")
	pending, _ := cmd.Flags().GetBool("pending")
	kinds, _ := cmd.Flags().GetStringSlice("kind")
	limit, _ := cmd.Flags().GetUint32("limit")
	offset, _ := cmd.Flags().GetUint32("offset")

	view, err := parseListView(viewStr)
	if err != nil {
		return err
	}

	// Reject activity-only flags when a different view is requested so
	// the agent can't silently no-op a filter.
	if view != walletrpc.ListView_LIST_VIEW_ACTIVITY {
		if pending {
			return fmt.Errorf("--pending applies only to --view "+
				"activity (got %q)", viewStr)
		}
		if len(kinds) > 0 {
			return fmt.Errorf("--kind applies only to --view "+
				"activity (got %q)", viewStr)
		}
	}

	req := &walletrpc.ListRequest{
		View:        view,
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
				return fmt.Errorf("list: %w", err)
			}

			return printWalletProto(resp)
		},
	)
}
