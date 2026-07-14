package waveclicommands

import (
	"context"
	"fmt"
	"strings"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/protobuf/proto"
)

// newOORCmd creates the oor parent command with receive-target subcommands.
func newOORCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "oor",
		Short: "Out-of-round operations",
		Long: "Manage out-of-round receive targets and other direct " +
			"transfer helpers.",
	}

	cmd.AddCommand(
		newOORGetCmd(),
		newOORListCmd(),
		newOORReceiveCmd(),
	)

	return cmd
}

// newOORGetCmd creates the oor get subcommand.
func newOORGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Get one OOR session status",
		RunE:  oorGet,
	}

	cmd.Flags().String("session-id", "",
		"OOR session id to fetch")

	// Accept the snake_case spelling (--session_id) as well as the
	// registered kebab form. The RPC field, `oor list` JSON output, and the
	// daemon logs all name this field session_id, so a user copying that
	// name should not hit an "unknown flag" error (issue #900). This only
	// adds an alias: every flag on this command (and the inherited global
	// flags) is defined in kebab case, so folding underscores to dashes
	// never renames an existing flag.
	cmd.Flags().SetNormalizeFunc(snakeToKebabFlags)

	return cmd
}

// snakeToKebabFlags folds a snake_case flag name onto its canonical kebab-case
// spelling so both spellings resolve to the same flag. Apply it only to
// commands whose flags are all defined in kebab case, where it acts purely as
// an alias.
func snakeToKebabFlags(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	return pflag.NormalizedName(strings.ReplaceAll(name, "_", "-"))
}

// newOORListCmd creates the oor list subcommand.
func newOORListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List OOR session statuses",
		RunE:  oorList,
	}

	cmd.Flags().String("direction", "all",
		"direction filter: all, outgoing, or incoming")
	cmd.Flags().String("status", "all",
		"status filter: all, pending, completed, or failed")
	cmd.Flags().Int32("page-size", 0,
		"maximum number of sessions to return")
	cmd.Flags().String("page-token", "",
		"cursor from a previous response for pagination")
	addListOutputFlags(cmd, "OOR session")

	return cmd
}

// newOORReceiveCmd creates the oor receive subcommand.
func newOORReceiveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "receive",
		Short: "Allocate a fresh receive script",
		Long: "Allocates a fresh wallet key, registers the matching " +
			"taproot receive script with the indexer, and " +
			"prints the resulting destination details.",
		RunE: oorReceive,
	}

	cmd.Flags().String("label", "",
		"optional label stored with the receive-script registration")

	return cmd
}

// oorGet executes the GetOORSession RPC.
func oorGet(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	req := &waverpc.GetOORSessionRequest{}
	if err := parseRequest(cmd, req, func() error {
		sessionID, _ := cmd.Flags().GetString("session-id")
		req.SessionId = sessionID

		return nil
	}); err != nil {
		return err
	}

	resp, err := client.GetOORSession(context.Background(), req)
	if err != nil {
		return fmt.Errorf("GetOORSession RPC failed: %w", err)
	}

	return printJSON(resp)
}

// oorList executes the ListOORSessions RPC.
func oorList(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	req := &waverpc.ListOORSessionsRequest{}
	if err := parseRequest(cmd, req, func() error {
		direction, _ := cmd.Flags().GetString("direction")
		directionFilter, err := parseOORDirectionFilter(direction)
		if err != nil {
			return err
		}
		req.DirectionFilter = directionFilter

		status, _ := cmd.Flags().GetString("status")
		statusFilter, err := parseOORStatusFilter(status)
		if err != nil {
			return err
		}
		req.StatusFilter = statusFilter

		pageSize, _ := cmd.Flags().GetInt32("page-size")
		req.PageSize = pageSize

		pageToken, _ := cmd.Flags().GetString("page-token")
		req.PageToken = pageToken

		return nil
	}); err != nil {
		return err
	}

	resp, err := client.ListOORSessions(context.Background(), req)
	if err != nil {
		return fmt.Errorf("ListOORSessions RPC failed: %w", err)
	}

	items := make([]proto.Message, len(resp.Sessions))
	for i, s := range resp.Sessions {
		items[i] = s
	}

	return renderListOutput(cmd, resp, items)
}

// oorReceive executes the NewReceiveScript RPC.
func oorReceive(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	req := &waverpc.NewReceiveScriptRequest{}
	if err := parseRequest(cmd, req, func() error {
		label, _ := cmd.Flags().GetString("label")
		req.Label = label

		return nil
	}); err != nil {
		return err
	}

	resp, err := client.NewReceiveScript(
		context.Background(), req,
	)
	if err != nil {
		return fmt.Errorf("NewReceiveScript RPC failed: %w", err)
	}

	return printJSON(resp)
}

// parseOORDirectionFilter converts a CLI direction filter into the proto enum.
func parseOORDirectionFilter(direction string) (waverpc.OORSessionDirection,
	error) {

	unspecified := waverpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_UNSPECIFIED
	outgoing := waverpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING
	incoming := waverpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING

	switch direction {
	case "", "all":
		return unspecified, nil

	case "outgoing":
		return outgoing, nil

	case "incoming":
		return incoming, nil

	default:
		return unspecified, fmt.Errorf("unknown OOR direction "+
			"filter: %s", direction)
	}
}

// parseOORStatusFilter converts a CLI status filter into the proto enum.
func parseOORStatusFilter(status string) (waverpc.OORSessionStatus, error) {
	unspecified := waverpc.OORSessionStatus_OOR_SESSION_STATUS_UNSPECIFIED
	pending := waverpc.OORSessionStatus_OOR_SESSION_STATUS_PENDING
	completed := waverpc.OORSessionStatus_OOR_SESSION_STATUS_COMPLETED
	failed := waverpc.OORSessionStatus_OOR_SESSION_STATUS_FAILED

	switch status {
	case "", "all":
		return unspecified, nil

	case "pending":
		return pending, nil

	case "completed":
		return completed, nil

	case "failed":
		return failed, nil

	default:
		return unspecified, fmt.Errorf("unknown OOR status filter: %s",
			status)
	}
}
