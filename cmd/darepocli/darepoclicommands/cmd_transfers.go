package darepoclicommands

import (
	"fmt"
	"strings"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
)

// newTransfersCmd creates the transfer status command group.
func newTransfersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "transfers",
		Short: "Inspect Ark transfer status",
		Long: "Lists user-facing in-round and out-of-round transfer " +
			"status assembled from local round, OOR, and " +
			"history data.",
	}

	cmd.AddCommand(newTransfersListCmd())

	return cmd
}

// newTransfersListCmd creates the transfers list subcommand.
func newTransfersListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List transfer statuses",
		Long: strings.Join([]string{
			"Lists in-round and out-of-round transfer status.",
			"When a direction filter is set, live in-round rows",
			"whose direction is still unknown are still returned.",
			"They remain visible because those rows can be useful",
			"before the daemon has enough local outpoint context",
			"to classify them as incoming or outgoing.",
		}, " "),
		RunE: transfersList,
	}

	cmd.Flags().String("mode", "all",
		"mode filter: all, inround, or oor")
	cmd.Flags().String("direction", "all",
		"direction filter: all, outgoing, or incoming; pending "+
			"in-round rows with unknown direction are always shown")
	cmd.Flags().String("status", "all",
		"status filter: all, pending, completed, or failed")
	cmd.Flags().Uint32("limit", 100,
		"maximum number of transfers to return (default 100)")
	cmd.Flags().Uint32("offset", 0,
		"number of filtered transfers to skip")

	return cmd
}

// transfersList executes the ListTransfers RPC.
func transfersList(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	req := &daemonrpc.ListTransfersRequest{}
	if err := parseRequest(cmd, req, func() error {
		mode, _ := cmd.Flags().GetString("mode")
		modeFilter, err := parseTransferModeFilter(mode)
		if err != nil {
			return err
		}
		req.ModeFilter = modeFilter

		direction, _ := cmd.Flags().GetString("direction")
		directionFilter, err := parseTransferDirectionFilter(direction)
		if err != nil {
			return err
		}
		req.DirectionFilter = directionFilter

		status, _ := cmd.Flags().GetString("status")
		statusFilter, err := parseTransferStatusFilter(status)
		if err != nil {
			return err
		}
		req.StatusFilter = statusFilter

		limit, _ := cmd.Flags().GetUint32("limit")
		req.Limit = limit

		offset, _ := cmd.Flags().GetUint32("offset")
		req.Offset = offset

		return nil
	}); err != nil {
		return err
	}

	resp, err := client.ListTransfers(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("ListTransfers RPC failed: %w", err)
	}

	return printJSON(resp)
}

// parseTransferModeFilter converts a CLI mode filter into the proto enum.
func parseTransferModeFilter(mode string) (daemonrpc.TransferMode, error) {
	unspecified := daemonrpc.TransferMode_TRANSFER_MODE_UNSPECIFIED

	switch mode {
	case "", "all":
		return unspecified, nil

	case "inround":
		return daemonrpc.TransferMode_TRANSFER_MODE_INROUND, nil

	case "oor":
		return daemonrpc.TransferMode_TRANSFER_MODE_OOR, nil

	default:
		return unspecified, fmt.Errorf("unknown transfer mode "+
			"filter: %s", mode)
	}
}

// parseTransferDirectionFilter converts a CLI direction filter into the proto
// enum.
func parseTransferDirectionFilter(direction string) (
	daemonrpc.TransferDirection, error) {

	const unspecified = daemonrpc.
		TransferDirection_TRANSFER_DIRECTION_UNSPECIFIED

	switch direction {
	case "", "all":
		return unspecified, nil

	case "outgoing":
		return daemonrpc.
			TransferDirection_TRANSFER_DIRECTION_OUTGOING, nil

	case "incoming":
		return daemonrpc.
			TransferDirection_TRANSFER_DIRECTION_INCOMING, nil

	default:
		return unspecified, fmt.Errorf("unknown transfer direction "+
			"filter: %s", direction)
	}
}

// parseTransferStatusFilter converts a CLI status filter into the proto enum.
func parseTransferStatusFilter(status string) (daemonrpc.TransferStatus,
	error) {

	unspecified := daemonrpc.TransferStatus_TRANSFER_STATUS_UNSPECIFIED

	switch status {
	case "", "all":
		return unspecified, nil

	case "pending":
		return daemonrpc.TransferStatus_TRANSFER_STATUS_PENDING, nil

	case "completed":
		return daemonrpc.TransferStatus_TRANSFER_STATUS_COMPLETED, nil

	case "failed":
		return daemonrpc.TransferStatus_TRANSFER_STATUS_FAILED, nil

	default:
		return unspecified, fmt.Errorf("unknown transfer status "+
			"filter: %s", status)
	}
}
