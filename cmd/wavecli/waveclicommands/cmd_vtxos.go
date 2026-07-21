package waveclicommands

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
// `waverpc.VTXOStatus` minus the UNSPECIFIED zero value; see
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

	addListOutputFlags(cmd, "VTXO")

	return cmd
}

// vtxosList executes the ListVTXOs RPC with optional filters.
func vtxosList(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &waverpc.ListVTXOsRequest{}
	if err := parseRequest(cmd, req, func() error {
		statusStr, _ := cmd.Flags().GetString("status")
		minAmount, _ := cmd.Flags().GetInt64("min_amount")

		if statusStr != "" {
			statusFilter, ok := parseVTXOStatus(
				statusStr,
			)
			if !ok {
				return PrintError(
					"INVALID_STATUS",
					fmt.Sprintf(
						"invalid status %q, valid: %s",
						statusStr, strings.Join(
							validStatuses, ", ",
						),
					),
				)
			}

			req.StatusFilter = statusFilter
		}

		req.MinAmountSat = minAmount

		return nil
	}); err != nil {
		return err
	}

	// cmd.Context() carries Ctrl+C / SIGTERM so the RPC actually
	// cancels when the user interrupts the CLI. context.Background()
	// here would orphan the request until the daemon responded.
	resp, err := client.ListVTXOs(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("ListVTXOs RPC failed: %w", err)
	}

	items := make([]proto.Message, len(resp.Vtxos))
	for i, v := range resp.Vtxos {
		items[i] = v
	}

	return renderListOutput(cmd, resp, items)
}

// parseVTXOStatus converts a status string to the proto enum.
func parseVTXOStatus(s string) (waverpc.VTXOStatus, bool) {
	normalized := strings.ToUpper(s)
	if !strings.HasPrefix(normalized, "VTXO_STATUS_") {
		normalized = "VTXO_STATUS_" + normalized
	}

	val, ok := waverpc.VTXOStatus_value[normalized]
	if !ok {
		return 0, false
	}

	return waverpc.VTXOStatus(val), true
}

// newVTXOsRefreshCmd creates the vtxos refresh subcommand.
func newVTXOsRefreshCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Queue VTXOs for refresh and join the next round",
		Long: "Queues one or more VTXOs for refresh and then " +
			"commits the queued intents to the next round, " +
			"extending the VTXOs' expiry.\n\n" +
			"A refresh is charged an operator fee, set by the " +
			"server-issued quote at seal time. Pass " +
			"`--dry_run` to preview an itemized advisory " +
			"estimate without queuing anything. An " +
			"interactive refresh shows the estimate and asks " +
			"for confirmation; pass `--yes` to skip the " +
			"prompt (required on non-interactive stdin).\n\n" +
			"Under the hood the refresh intent first " +
			"accumulates in the round actor's " +
			"PendingRoundAssembly state. By default this " +
			"command then issues `ark rounds join` on the " +
			"caller's behalf so refresh is a one-shot " +
			"operation. Pass `--no_join` to skip the join — " +
			"useful for batching multiple refresh and/or " +
			"leave RPCs into the same round (one shared " +
			"change marker, one shared commitment fee). When " +
			"`--no_join` is used, call `ark rounds join` " +
			"explicitly once the batch is queued.",
		RunE: vtxosRefresh,
	}

	cmd.Flags().StringSlice("outpoint", nil,
		"VTXO outpoint(s) to refresh (txid:index)")

	cmd.Flags().Bool("all", false,
		"refresh all live VTXOs")

	cmd.Flags().Bool("dry_run", false,
		"validate without queuing and preview the estimated "+
			"operator fee")

	cmd.Flags().Bool("yes", false,
		"skip the interactive fee confirmation")

	cmd.Flags().Bool("no_join", false,
		"skip the implicit `ark rounds join` follow-up so this "+
			"refresh can batch with other queued intents")

	return cmd
}

// buildRefreshVTXOsRequest translates the flag surface into a
// RefreshVTXOsRequest. Extracted so the MCP tool and the CLI share
// the same selection guards — divergence here would let one surface
// silently refresh a different VTXO set than the other.
func buildRefreshVTXOsRequest(outpoints []string, all,
	dryRun bool) (*waverpc.RefreshVTXOsRequest, error) {

	if !all && len(outpoints) == 0 {
		return nil, fmt.Errorf("either --outpoint or --all is required")
	}

	if all && len(outpoints) > 0 {
		return nil, fmt.Errorf("--outpoint and --all are mutually " +
			"exclusive")
	}

	req := &waverpc.RefreshVTXOsRequest{DryRun: dryRun}

	if all {
		req.Selection = &waverpc.RefreshVTXOsRequest_All{
			All: true,
		}
	} else {
		req.Selection = &waverpc.RefreshVTXOsRequest_Outpoints{
			Outpoints: &waverpc.OutpointSelection{
				Outpoints: outpoints,
			},
		}
	}

	return req, nil
}

// vtxosRefresh executes the RefreshVTXOs RPC. Flag handling lives in
// buildRefreshVTXOsRequest so the same builder can be reused by the
// MCP tool and covered with a focused unit test.
func vtxosRefresh(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &waverpc.RefreshVTXOsRequest{}
	if err := parseRequest(cmd, req, func() error {
		outpoints, _ := cmd.Flags().GetStringSlice(
			"outpoint",
		)
		all, _ := cmd.Flags().GetBool("all")
		dryRun, _ := cmd.Flags().GetBool("dry_run")

		built, err := buildRefreshVTXOsRequest(
			outpoints, all, dryRun,
		)
		if err != nil {
			return err
		}

		// Proto pointer-receiver shims: copy built fields onto
		// the outer req that parseRequest owns.
		req.Selection = built.Selection
		req.DryRun = built.DryRun

		return nil
	}); err != nil {
		return err
	}

	if err := confirmRefreshIfNeeded(
		cmd, req, func(preview *waverpc.RefreshVTXOsRequest) (
			*waverpc.RefreshVTXOsResponse, error) {

			return client.RefreshVTXOs(cmd.Context(), preview)
		},
	); err != nil {
		return err
	}

	resp, err := client.RefreshVTXOs(
		cmd.Context(), req,
	)
	if err != nil {
		return fmt.Errorf("RefreshVTXOs RPC failed: %w", err)
	}

	if err := printJSON(resp); err != nil {
		return err
	}

	// The itemized estimate is in the JSON body on stdout; the
	// summary goes to stderr so a human reading the terminal sees
	// the headline number without stdout consumers having to strip
	// prose.
	for _, line := range summarizeRefreshFeeEstimate(
		resp.GetFeeEstimate(),
	) {
		fmt.Fprintln(cmd.ErrOrStderr(), line)
	}

	// A non-empty dry-run preview without an estimate can only come
	// from a daemon that predates the fee preview: the daemon omits
	// the estimate solely for empty selections. Warn rather than
	// stay silent — the flag help promises a fee preview, and
	// silence here would read as "free".
	if req.DryRun && len(resp.GetQueuedOutpoints()) > 0 &&
		resp.GetFeeEstimate() == nil {

		fmt.Fprintln(
			cmd.ErrOrStderr(),
			"warning: the daemon returned no fee estimate "+
				"(older daemon?); a refresh is still "+
				"charged the binding seal-time fee",
		)
	}

	noJoin, _ := cmd.Flags().GetBool("no_join")

	return maybeJoinNextRound(cmd, client, req.DryRun, noJoin)
}

// summarizeRefreshFeeEstimate renders the dry-run fee estimate as
// human-readable stderr lines. Split from the IO wrapper so the
// wording rules are covered by a focused unit test. A nil estimate
// (real refresh, empty selection) renders nothing.
//
// Every branch repeats that the number is advisory: the binding fee is
// set by the seal-time quote, and telling the user otherwise here
// would overpromise exactly the guarantee the refresh flow cannot
// make.
func summarizeRefreshFeeEstimate(est *waverpc.RefreshFeeEstimate) []string {
	if est == nil {
		return nil
	}

	var lines []string

	count := len(est.Outpoints)

	switch {
	case est.EstimateError != "":
		lines = append(
			lines, fmt.Sprintf("warning: refresh fee estimate "+
				"unavailable (%s); a refresh is still "+
				"charged the binding seal-time fee",
				est.EstimateError),
		)

		if est.FreeRefreshEligible {
			lines = append(
				lines, fmt.Sprintf("expected free: all %d "+
					"selected VTXO(s) are inside the "+
					"operator's free-refresh window "+
					"(advisory)", count),
			)
		}

	case est.FreeRefreshEligible:
		lines = append(
			lines, fmt.Sprintf("estimated refresh fee: 0 sat — "+
				"all %d selected VTXO(s) are inside the "+
				"operator's free-refresh window (advisory; "+
				"the binding fee is set at seal time)", count),
		)

	default:
		lines = append(
			lines,
			fmt.Sprintf(
				"estimated refresh fee: %d sat across %d "+
					"VTXO(s) (advisory; the binding fee "+
					"is set at seal time)",
				est.GetEstimatedTotalFeeSat(), count,
			),
		)
	}

	var belowDust int
	for _, row := range est.Outpoints {
		if row.BelowDustWarning {
			belowDust++
		}
	}
	if belowDust > 0 {
		lines = append(
			lines, fmt.Sprintf("warning: %d selected VTXO(s) are "+
				"below the operator's minimum viable amount; "+
				"refreshing them pays fees on coins that "+
				"already cost more to exit than they hold",
				belowDust),
		)
	}

	return lines
}

// confirmRefreshIfNeeded gates dispatch of a real refresh on explicit
// consent: every refresh is charged an operator fee at seal time, and
// before this gate the CLI queued the refresh without ever mentioning
// it (#986). The check runs after parseRequest returns so it covers
// both the flag-driven path and the --json path, mirroring
// confirmLeaveAllIfNeeded. Skip when --yes is set (explicit scripted
// consent) or when req.DryRun is set (previewing, not spending).
//
// When stdin is not a TTY (agents, CI, pipelines), the function
// refuses to prompt — the caller must pass --yes or --dry_run — so a
// non-interactive invocation is never blocked on a y/N it cannot
// answer. Only on a TTY does it fetch the advisory estimate (via the
// same RPC in dry-run form, through fetchPreview) and prompt, so the
// operator consents to a number rather than a mystery fee. A preview
// that fails with InvalidArgument aborts — the real refresh would
// reject the same request shape — while any other preview failure
// degrades to prompting with an "estimate unavailable" warning: a
// broken estimate path must not make refreshes unconfirmable.
func confirmRefreshIfNeeded(cmd *cobra.Command,
	req *waverpc.RefreshVTXOsRequest,
	fetchPreview func(*waverpc.RefreshVTXOsRequest) (
		*waverpc.RefreshVTXOsResponse, error)) error {

	if req.DryRun {
		return nil
	}

	confirmYes, _ := cmd.Flags().GetBool("yes")
	if confirmYes {
		return nil
	}

	if !canPrompt(cmd) {
		return PrintError(
			"INVALID_ARGS", "refresh is charged an operator "+
				"fee at seal time and requires --yes "+
				"(explicit consent) or --dry_run (fee "+
				"preview) on non-interactive stdin or when "+
				"input is disabled; refusing to prompt "+
				"because an agent cannot respond to y/N",
		)
	}

	cloned := proto.Clone(req)
	preview, ok := cloned.(*waverpc.RefreshVTXOsRequest)
	if !ok {

		// Unreachable: proto.Clone preserves the concrete type.
		return fmt.Errorf("unexpected preview request type %T", cloned)
	}
	preview.DryRun = true

	var lines []string
	resp, err := fetchPreview(preview)
	switch {
	case status.Code(err) == codes.InvalidArgument:
		// The dry-run validates the same request shape the real
		// dispatch would; a caller mistake (unknown or malformed
		// outpoint) should abort here, not after a confirmed
		// queue.
		return fmt.Errorf("refresh preview rejected: %w", err)

	case err != nil:
		lines = []string{
			"warning: refresh fee estimate unavailable " +
				"(preview failed); a refresh is still " +
				"charged the binding seal-time fee",
		}

	default:
		// An empty preview means the selection resolves to
		// nothing (refresh --all with no live VTXOs): the real
		// dispatch queues nothing and charges nothing, so there
		// is no fee to consent to and warning about a charge
		// would be false.
		if resp.GetStatus() == "preview" &&
			len(resp.GetQueuedOutpoints()) == 0 {

			fmt.Fprintln(
				cmd.ErrOrStderr(),
				"no live VTXOs to refresh; nothing will be "+
					"queued",
			)

			return nil
		}

		lines = summarizeRefreshFeeEstimate(resp.GetFeeEstimate())
		if len(lines) == 0 {
			lines = []string{
				"warning: the daemon returned no fee " +
					"estimate; a refresh is still " +
					"charged the binding seal-time fee",
			}
		}
	}

	return promptRefreshConfirmation(cmd, lines)
}

// promptRefreshConfirmation shows the fee-estimate lines and asks the
// operator to confirm the refresh on stdin. Any answer other than "y"
// or "yes" (case-insensitive) aborts the command. Only called when
// stdin is known to be a TTY (see confirmRefreshIfNeeded). All prompt
// output goes to stderr: stdout is reserved for the JSON body, and a
// prompt swallowed by a shell pipe would read as a hung command.
func promptRefreshConfirmation(cmd *cobra.Command, lines []string) error {
	out := cmd.ErrOrStderr()

	for _, line := range lines {
		fmt.Fprintln(out, line)
	}

	return promptConfirmation(cmd, "Proceed with refresh? [y/N]: ")
}

// newVTXOsLeaveCmd creates the vtxos leave subcommand.
func newVTXOsLeaveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "leave",
		Short: "Queue VTXOs for cooperative leave and join the " +
			"next round",
		Long: "Queues one or more VTXOs for cooperative leave " +
			"and then commits the queued intents to the next " +
			"round. Each forfeited VTXO pays out (amount - " +
			"operator_fee) to the specified on-chain " +
			"destination.\n\n" +
			"Under the hood the leave intent first " +
			"accumulates in PendingRoundAssembly alongside " +
			"any queued refresh intents. By default this " +
			"command then issues `ark rounds join` on the " +
			"caller's behalf so leave is a one-shot " +
			"operation. Pass `--no_join` to skip the join — " +
			"useful for batching multiple leave and/or " +
			"refresh RPCs into the same round (one shared " +
			"change marker, one shared commitment fee). When " +
			"`--no_join` is used, call `ark rounds join` " +
			"explicitly once the batch is queued.",
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

	cmd.Flags().Bool("no_join", false,
		"skip the implicit `ark rounds join` follow-up so this "+
			"leave can batch with other queued intents")

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

	req := &waverpc.LeaveVTXOsRequest{}
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
		cmd.Context(), req,
	)
	if err != nil {
		return fmt.Errorf("LeaveVTXOs RPC failed: %w", err)
	}

	if err := printJSON(resp); err != nil {
		return err
	}

	noJoin, _ := cmd.Flags().GetBool("no_join")

	return maybeJoinNextRound(cmd, client, req.DryRun, noJoin)
}

// autoJoinDecision is the outcome of evaluating whether to invoke
// JoinNextRound after a refresh / leave queue RPC.
type autoJoinDecision struct {
	// Join is true when the implicit JoinNextRound RPC must fire.
	Join bool

	// Notice is the stderr line that explains the decision so a
	// human reading the terminal can see whether the round was
	// auto-joined, deferred (--no_join), or skipped (--dry_run).
	Notice string
}

// decideAutoJoin captures the policy for whether refresh / leave should
// auto-join the next round. Split from the IO wrapper so the policy is
// covered by a focused unit test without needing a gRPC fixture.
func decideAutoJoin(dryRun, noJoin bool) autoJoinDecision {
	switch {
	case dryRun:
		return autoJoinDecision{
			Notice: "dry run: skipping `ark rounds join`",
		}

	case noJoin:
		return autoJoinDecision{
			Notice: "--no_join set: intents queued; " +
				"run `ark rounds join` to commit",
		}

	default:
		return autoJoinDecision{
			Join: true,
			Notice: "auto-joined next round; " +
				"pass --no_join to skip",
		}
	}
}

// maybeJoinNextRound issues the JoinNextRound RPC unless the caller
// opted out via --no_join or asked for a dry run. The auto-join makes
// refresh / leave one-shot operations by default while preserving the
// batched workflow via --no_join. A short status line goes to stderr so
// stdout JSON stays parseable by piped consumers.
func maybeJoinNextRound(cmd *cobra.Command, client waverpc.DaemonServiceClient,
	dryRun, noJoin bool) error {

	decision := decideAutoJoin(dryRun, noJoin)

	if decision.Join {
		resp, err := client.JoinNextRound(
			cmd.Context(), &waverpc.JoinNextRoundRequest{},
		)
		if err != nil {
			return fmt.Errorf("auto-join next round failed: %w",
				err)
		}

		// A refresh or leave that queued nothing (e.g. --all with no
		// live VTXOs) leaves no pending round to join; the daemon
		// reports this benign no-op rather than failing, so say so
		// plainly instead of the ordinary auto-join notice, which
		// would wrongly imply a round was joined.
		if resp.GetStatus() == "nothing_to_join" {
			fmt.Fprintln(
				cmd.ErrOrStderr(),
				"nothing queued to join",
			)

			return nil
		}
	}

	fmt.Fprintln(cmd.ErrOrStderr(), decision.Notice)

	return nil
}

// buildLeaveVTXOsRequest translates the flag surface into a
// LeaveVTXOsRequest. Extracted so the MCP tool and the CLI share
// the same destination-parsing logic — divergence here would let
// the two surfaces offboard to subtly different scripts.
func buildLeaveVTXOsRequest(outpoints []string, all bool, addr,
	pkScriptHex string, destPairs map[string]string,
	dryRun bool) (*waverpc.LeaveVTXOsRequest, error) {

	if !all && len(outpoints) == 0 {
		return nil, fmt.Errorf("either --outpoint or --all is required")
	}

	if all && len(outpoints) > 0 {
		return nil, fmt.Errorf("--outpoint and --all are mutually " +
			"exclusive")
	}

	req := &waverpc.LeaveVTXOsRequest{DryRun: dryRun}

	if all {
		req.Selection = &waverpc.LeaveVTXOsRequest_All{
			All: true,
		}
	} else {
		req.Selection = &waverpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &waverpc.OutpointSelection{
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
		req.DefaultDestination = &waverpc.LeaveDestination{
			Target: &waverpc.LeaveDestination_Address{
				Address: addr,
			},
		}

	case pkScriptHex != "":
		raw, err := hex.DecodeString(pkScriptHex)
		if err != nil {
			return nil, fmt.Errorf("invalid --pk_script hex: %w",
				err)
		}

		req.DefaultDestination = &waverpc.LeaveDestination{
			Target: &waverpc.LeaveDestination_PkScript{
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
			map[string]*waverpc.LeaveDestination, len(destPairs),
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
func parseDestinationValue(raw string) (*waverpc.LeaveDestination, error) {
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

		return &waverpc.LeaveDestination{
			Target: &waverpc.LeaveDestination_PkScript{
				PkScript: bs,
			},
		}, nil
	}

	return &waverpc.LeaveDestination{
		Target: &waverpc.LeaveDestination_Address{
			Address: raw,
		},
	}, nil
}

// confirmLeaveAllIfNeeded gates dispatch on an explicit confirmation
// when the resolved request selects all VTXOs. The check runs after
// parseRequest returns so it covers both the flag-driven path and the
// --json path; previously the prompt lived inside the flag callback
// and an agent piping `{"all":true,...}` would silently skip the gate
// that the bespoke-flag path enforced. Skip when --yes is set
// (explicit scripted consent), when req.DryRun is set (just
// previewing, not moving funds), or when WAVECLI_AGENT_MODE=1 is
// rejected entirely.
//
// When stdin is not a TTY (agents, CI, pipelines), the function
// refuses to prompt: an interactive y/N read would block forever or
// race against piped stdin. The caller must pass --yes or --dry_run
// explicitly. Only when stdin IS a TTY does the function fall back
// to the interactive prompt for ergonomics during operator use.
func confirmLeaveAllIfNeeded(cmd *cobra.Command,
	req *waverpc.LeaveVTXOsRequest) error {

	_, isAll := req.Selection.(*waverpc.LeaveVTXOsRequest_All)
	if !isAll {
		return nil
	}

	confirmYes, _ := cmd.Flags().GetBool("yes")
	if confirmYes || req.DryRun {
		return nil
	}

	// Stdin is not a TTY — refuse to prompt rather than block or
	// race agents whose stdin is closed / piped. The agent-cli
	// skill lists interactive prompts as an anti-pattern; this
	// guard is the explicit replacement.
	if !canPrompt(cmd) {
		return PrintError(
			"INVALID_ARGS", "--all requires --yes (explicit "+
				"consent) or --dry_run (preview) on "+
				"non-interactive stdin or when input is "+
				"disabled; refusing to prompt because an "+
				"agent cannot respond to y/N",
		)
	}

	return promptLeaveAllConfirmation(cmd)
}

// stdinIsTTY is a package-level indirection over the TTY detection
// so tests can deterministically exercise both branches without
// depending on the process's actual stdin (which inherits from `go
// test`'s terminal in some setups and a pipe in others).
var stdinIsTTY = defaultStdinIsTTY

// defaultStdinIsTTY reports whether the y/N prompt is safe to drive
// on the given command, returning true when the prompt may proceed
// and false when the caller must pass --yes or --dry_run instead.
//
// Two paths return true (prompt is safe):
//
//  1. cmd.InOrStdin() returns something other than os.Stdin. Only
//     tests and embedded harnesses install custom stdin via
//     cmd.SetIn; production agents do not, so a custom reader is
//     the caller's signal that they will drive the prompt
//     themselves (e.g. by piping a scripted "y\n").
//
//  2. cmd.InOrStdin() is os.Stdin and os.Stdin is an actual terminal
//     (per term.IsTerminal), i.e. a device where the operator can
//     answer interactively.
//
// All other cases return false and the caller refuses to prompt. A
// character-device check is deliberately NOT used here: /dev/null and
// a closed descriptor are both character devices yet are not
// terminals, and those are the most common non-interactive stdin
// shapes for agents, CI, and systemd units. Treating them as a TTY
// would print a y/N prompt that immediately reads EOF, which is the
// exact hang-then-fail the consent gate exists to prevent.
func defaultStdinIsTTY(cmd *cobra.Command) bool {
	if cmd.InOrStdin() != os.Stdin {
		return true
	}

	return term.IsTerminal(int(os.Stdin.Fd()))
}

// promptLeaveAllConfirmation asks the operator to confirm a
// --all invocation on stdin. Any answer other than "y" or "yes"
// (case-insensitive) aborts the command. Only called when stdin is
// known to be a TTY (see confirmLeaveAllIfNeeded). The prompt goes to
// stderr, matching promptRefreshConfirmation: stdout is reserved for
// the JSON body, and a prompt swallowed by a shell pipe would read as
// a hung command.
func promptLeaveAllConfirmation(cmd *cobra.Command) error {
	out := cmd.ErrOrStderr()

	fmt.Fprintln(
		out, "About to queue ALL live VTXOs for cooperative leave. "+
			"This moves funds on-chain.",
	)

	return promptConfirmation(cmd, "Proceed? [y/N]: ")
}
