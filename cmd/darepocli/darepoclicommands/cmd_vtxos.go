package darepoclicommands

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

// newVTXOsCmd creates the vtxos parent command with subcommands for
// list and refresh.
func newVTXOsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vtxos",
		Short: "VTXO operations",
		Long: "List, filter, and manage virtual " +
			"transaction outputs (VTXOs).",
	}

	cmd.AddCommand(
		newVTXOsListCmd(), newVTXOsRefreshCmd(), newVTXOsLeaveCmd(),
	)

	return cmd
}

// validStatuses lists the valid VTXO status filter values for input
// validation and error messages. These must match the proto enum
// `daemonrpc.VTXOStatus` minus the UNSPECIFIED zero value; see
// `parseVTXOStatus` for the lookup that converts to the proto enum.
var validStatuses = []string{
	"live", "pending_forfeit", "forfeiting",
	"forfeited", "spent", "unilateral_exit", "failed",
	"spending",
}

// newVTXOsListCmd creates the vtxos list subcommand.
func newVTXOsListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List VTXOs",
		Long: "Returns VTXOs known to the wallet, " +
			"optionally filtered by status and " +
			"minimum amount.",
		RunE: vtxosList,
	}

	cmd.Flags().String("status", "",
		"filter by status: "+
			strings.Join(validStatuses, ", "))

	cmd.Flags().Int64("min_amount", 0,
		"minimum amount in sats")

	cmd.Flags().String("fields", "",
		"comma-separated field names to include")

	cmd.Flags().Bool("ndjson", false,
		"emit one JSON object per VTXO (newline-delimited)")

	return cmd
}

// vtxosList executes the ListVTXOs RPC with optional filters.
func vtxosList(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &daemonrpc.ListVTXOsRequest{}
	if err := parseRequest(cmd, req, func() error {
		statusStr, _ := cmd.Flags().GetString("status")
		minAmount, _ := cmd.Flags().GetInt64("min_amount")

		if statusStr != "" {
			statusFilter, ok := parseVTXOStatus(
				statusStr,
			)
			if !ok {
				PrintError(
					"INVALID_STATUS",
					fmt.Sprintf(
						"invalid status %q, valid: %s",
						statusStr, strings.Join(
							validStatuses, ", ",
						),
					),
				)

				return fmt.Errorf("invalid status filter: %s",
					statusStr)
			}

			req.StatusFilter = statusFilter
		}

		req.MinAmountSat = minAmount

		return nil
	}); err != nil {
		return err
	}

	resp, err := client.ListVTXOs(
		context.Background(), req,
	)
	if err != nil {
		return fmt.Errorf("ListVTXOs RPC failed: %w", err)
	}

	// Parse optional output modifiers.
	ndjson, _ := cmd.Flags().GetBool("ndjson")
	fieldsStr, _ := cmd.Flags().GetString("fields")

	// Emit newline-delimited JSON if --ndjson was specified. When
	// combined with --fields, each line is filtered to the
	// requested fields.
	if ndjson {
		items := make(
			[]proto.Message, len(resp.Vtxos),
		)
		for i, v := range resp.Vtxos {
			items[i] = v
		}

		if fieldsStr != "" {
			fields := strings.Split(fieldsStr, ",")

			return printNDJSONFields(items, fields)
		}

		return printNDJSON(items)
	}

	// Apply field mask if --fields was specified.
	if fieldsStr != "" {
		fields := strings.Split(fieldsStr, ",")

		return printJSONFields(resp, fields)
	}

	return printJSON(resp)
}

// parseVTXOStatus converts a status string to the proto enum.
func parseVTXOStatus(s string) (daemonrpc.VTXOStatus, bool) {
	normalized := strings.ToUpper(s)
	if !strings.HasPrefix(normalized, "VTXO_STATUS_") {
		normalized = "VTXO_STATUS_" + normalized
	}

	val, ok := daemonrpc.VTXOStatus_value[normalized]
	if !ok {
		return 0, false
	}

	return daemonrpc.VTXOStatus(val), true
}

// newVTXOsRefreshCmd creates the vtxos refresh subcommand.
func newVTXOsRefreshCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Queue VTXOs for refresh",
		Long: "Queues one or more VTXOs for refresh in " +
			"the next round, extending their expiry.",
		RunE: vtxosRefresh,
	}

	cmd.Flags().StringSlice("outpoint", nil,
		"VTXO outpoint(s) to refresh (txid:index)")

	cmd.Flags().Bool("all", false,
		"refresh all live VTXOs")

	cmd.Flags().Bool("dry_run", false,
		"validate without queuing")

	return cmd
}

// vtxosRefresh executes the RefreshVTXOs RPC.
func vtxosRefresh(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &daemonrpc.RefreshVTXOsRequest{}
	if err := parseRequest(cmd, req, func() error {
		outpoints, _ := cmd.Flags().GetStringSlice(
			"outpoint",
		)
		all, _ := cmd.Flags().GetBool("all")
		dryRun, _ := cmd.Flags().GetBool("dry_run")

		if !all && len(outpoints) == 0 {
			return fmt.Errorf("either --outpoint or --all is " +
				"required")
		}

		if all {
			req.Selection = &daemonrpc.RefreshVTXOsRequest_All{
				All: true,
			}
		} else {
			sel := &daemonrpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: outpoints,
				},
			}
			req.Selection = sel
		}
		req.DryRun = dryRun

		return nil
	}); err != nil {
		return err
	}

	resp, err := client.RefreshVTXOs(
		context.Background(), req,
	)
	if err != nil {
		return fmt.Errorf("RefreshVTXOs RPC failed: %w", err)
	}

	return printJSON(resp)
}

// newVTXOsLeaveCmd creates the vtxos leave subcommand.
func newVTXOsLeaveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "leave",
		Short: "Queue VTXOs for cooperative leave (offboard)",
		Long: "Queues one or more VTXOs for cooperative " +
			"leave in the next round. Each forfeited " +
			"VTXO pays out (amount - operator_fee) to " +
			"the specified on-chain destination.",
		RunE: vtxosLeave,
	}

	cmd.Flags().StringSlice("outpoint", nil,
		"VTXO outpoint(s) to leave (txid:index)")

	cmd.Flags().Bool("all", false,
		"leave all live VTXOs")

	cmd.Flags().String("address", "",
		"default on-chain destination address")

	cmd.Flags().String("pk_script", "",
		"default destination pk_script (hex); "+
			"alternative to --address")

	cmd.Flags().StringToString("destination", nil,
		"per-outpoint destination override: "+
			"outpoint=addr or outpoint=script:<hex>")

	cmd.Flags().Bool("dry_run", false,
		"validate without queuing")

	cmd.Flags().Bool("yes", false,
		"skip interactive confirmation for --all")

	return cmd
}

// vtxosLeave executes the LeaveVTXOs RPC. Flag handling lives in
// buildLeaveVTXOsRequest so the same builder can be reused by the
// MCP tool and covered with a focused unit test.
func vtxosLeave(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &daemonrpc.LeaveVTXOsRequest{}
	if err := parseRequest(cmd, req, func() error {
		outpoints, _ := cmd.Flags().GetStringSlice("outpoint")
		all, _ := cmd.Flags().GetBool("all")
		addr, _ := cmd.Flags().GetString("address")
		pkScriptHex, _ := cmd.Flags().GetString("pk_script")
		destPairs, _ := cmd.Flags().GetStringToString(
			"destination",
		)
		dryRun, _ := cmd.Flags().GetBool("dry_run")

		built, err := buildLeaveVTXOsRequest(
			outpoints, all, addr, pkScriptHex, destPairs, dryRun,
		)
		if err != nil {
			return err
		}

		// Proto pointer-receiver shims: copy built fields onto
		// the outer req that parseRequest owns.
		req.Selection = built.Selection
		req.DefaultDestination = built.DefaultDestination
		req.Destinations = built.Destinations
		req.DryRun = built.DryRun

		return nil
	}); err != nil {
		return err
	}

	if err := confirmLeaveAllIfNeeded(cmd, req); err != nil {
		return err
	}

	resp, err := client.LeaveVTXOs(
		context.Background(), req,
	)
	if err != nil {
		return fmt.Errorf("LeaveVTXOs RPC failed: %w", err)
	}

	return printJSON(resp)
}

// buildLeaveVTXOsRequest translates the flag surface into a
// LeaveVTXOsRequest. Extracted so the MCP tool and the CLI share
// the same destination-parsing logic — divergence here would let
// the two surfaces offboard to subtly different scripts.
func buildLeaveVTXOsRequest(outpoints []string, all bool, addr,
	pkScriptHex string, destPairs map[string]string,
	dryRun bool) (*daemonrpc.LeaveVTXOsRequest, error) {

	if !all && len(outpoints) == 0 {
		return nil, fmt.Errorf("either --outpoint or --all is required")
	}

	if all && len(outpoints) > 0 {
		return nil, fmt.Errorf("--outpoint and --all are mutually " +
			"exclusive")
	}

	req := &daemonrpc.LeaveVTXOsRequest{DryRun: dryRun}

	if all {
		req.Selection = &daemonrpc.LeaveVTXOsRequest_All{
			All: true,
		}
	} else {
		req.Selection = &daemonrpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &daemonrpc.OutpointSelection{
				Outpoints: outpoints,
			},
		}
	}

	// Default destination: at most one of --address / --pk_script.
	switch {
	case addr != "" && pkScriptHex != "":
		return nil, fmt.Errorf("--address and --pk_script are " +
			"mutually exclusive")

	case addr != "":
		req.DefaultDestination = &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_Address{
				Address: addr,
			},
		}

	case pkScriptHex != "":
		raw, err := hex.DecodeString(pkScriptHex)
		if err != nil {
			return nil, fmt.Errorf("invalid --pk_script hex: %w",
				err)
		}

		req.DefaultDestination = &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_PkScript{
				PkScript: raw,
			},
		}
	}

	// Per-outpoint overrides: key=outpoint, value=addr or
	// "script:<hex>". Overrides are only meaningful under
	// selection=outpoints; --all rejects the combination at the
	// daemon, but the CLI catches it earlier for a cleaner error.
	if len(destPairs) > 0 && all {
		return nil, fmt.Errorf("--destination overrides are not " +
			"supported with --all")
	}

	if len(destPairs) > 0 {
		req.Destinations = make(
			map[string]*daemonrpc.LeaveDestination, len(destPairs),
		)
		for op, raw := range destPairs {
			dest, err := parseDestinationValue(raw)
			if err != nil {
				return nil, fmt.Errorf("--destination[%s]: %w",
					op, err)
			}
			req.Destinations[op] = dest
		}
	}

	// A missing default without full per-outpoint coverage is
	// rejected server-side; we let the daemon be the source of
	// truth there rather than re-checking here (the daemon also
	// validates outpoint strings, which we don't parse on the CLI
	// side to avoid the btcd import weight).
	if req.DefaultDestination == nil && len(req.Destinations) == 0 {
		return nil, fmt.Errorf("either --address / --pk_script " +
			"(default destination) or --destination " +
			"(per-outpoint overrides) is required")
	}

	return req, nil
}

// parseDestinationValue parses a --destination value: either a raw
// address or a "script:<hex>" form carrying a pre-resolved pk_script.
func parseDestinationValue(raw string) (*daemonrpc.LeaveDestination, error) {
	if raw == "" {
		return nil, fmt.Errorf("empty destination value")
	}

	const scriptPrefix = "script:"
	if strings.HasPrefix(raw, scriptPrefix) {
		scriptHex := strings.TrimPrefix(raw, scriptPrefix)
		bs, err := hex.DecodeString(scriptHex)
		if err != nil {
			return nil, fmt.Errorf("invalid script hex: %w", err)
		}

		return &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_PkScript{
				PkScript: bs,
			},
		}, nil
	}

	return &daemonrpc.LeaveDestination{
		Target: &daemonrpc.LeaveDestination_Address{
			Address: raw,
		},
	}, nil
}

// confirmLeaveAllIfNeeded gates dispatch on an interactive y/N prompt
// when the resolved request selects all VTXOs. The check runs after
// parseRequest returns so it covers both the flag-driven path and the
// --json path; previously the prompt lived inside the flag callback
// and an agent piping `{"all":true,...}` would silently skip the gate
// that the bespoke-flag path enforced. Skip when --yes is set
// (scripted use) or when req.DryRun is set (just previewing, not
// moving funds).
func confirmLeaveAllIfNeeded(cmd *cobra.Command,
	req *daemonrpc.LeaveVTXOsRequest) error {

	_, isAll := req.Selection.(*daemonrpc.LeaveVTXOsRequest_All)
	if !isAll {
		return nil
	}

	confirmYes, _ := cmd.Flags().GetBool("yes")
	if confirmYes || req.DryRun {
		return nil
	}

	return promptLeaveAllConfirmation(cmd)
}

// promptLeaveAllConfirmation asks the operator to confirm a
// --all invocation on stdin. Any answer other than "y" or "yes"
// (case-insensitive) aborts the command.
func promptLeaveAllConfirmation(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	fmt.Fprintln(
		out, "About to queue ALL live VTXOs for cooperative leave. "+
			"This moves funds on-chain.",
	)
	fmt.Fprint(out, "Proceed? [y/N]: ")

	reader := bufio.NewReader(cmd.InOrStdin())
	answer, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}

	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		return fmt.Errorf("aborted by user")
	}

	return nil
}
