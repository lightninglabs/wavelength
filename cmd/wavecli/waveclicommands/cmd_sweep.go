package waveclicommands

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

// newSweepCmd creates the sweep subcommand for recovering expired boarding
// UTXOs through their unilateral timeout path.
func newSweepCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "Sweep expired boarding UTXOs",
		Long: "Scans boarding UTXOs whose CSV timeout path has " +
			"matured and prints the recoverable amount, " +
			"estimated fee, and net amount. If no mature " +
			"outputs are available, the command returns an " +
			"empty preview. Pass --broadcast to publish one " +
			"aggregate sweep transaction. The daemon records " +
			"the sweep as pending and marks the boarding " +
			"outputs swept after confirmed spends.",
		RunE: sweep,
	}
	cmd.AddCommand(newSweepListCmd())

	cmd.Flags().StringSlice("outpoint", nil,
		"boarding UTXO outpoint to sweep (txid:index); repeatable")
	cmd.Flags().Bool("broadcast", false,
		"broadcast aggregate sweep and track it until confirmed spent")
	cmd.Flags().Int64("fee-rate-sat-per-vbyte", 0,
		"fee rate override in sat/vbyte; zero estimates by conf target")
	cmd.Flags().Uint32("conf-target", 0,
		"confirmation target for fee estimation; zero uses default")
	cmd.Flags().String("sweep-address", "",
		"optional sweep destination; empty uses a fresh wallet address")

	return cmd
}

// newSweepListCmd creates the subcommand for listing tracked sweep
// transactions.
func newSweepListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tracked boarding sweeps",
		Long: "Lists aggregate boarding sweep transactions persisted " +
			"by the daemon, including their lifecycle status, " +
			"fee, confirmation height, and per-input spend " +
			"state.",
		RunE: sweepList,
	}

	cmd.Flags().String("status", "",
		"optional status filter: pending, published, confirmed, "+
			"external_resolved, or failed")
	cmd.Flags().Uint32("page-size", 0,
		"maximum tracked sweeps to return; zero uses the "+
			"daemon default")
	cmd.Flags().String("page-token", "",
		"page token returned by a previous sweep list response")
	addListOutputFlags(cmd, "sweep")

	return cmd
}

// sweep executes the SweepBoardingUTXOs RPC and prints the result.
func sweep(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	req := &waverpc.SweepBoardingUTXOsRequest{}
	fromFlags := func() error {
		outpoints, _ := cmd.Flags().GetStringSlice("outpoint")
		broadcast, _ := cmd.Flags().GetBool("broadcast")
		feeRate, _ := cmd.Flags().GetInt64("fee-rate-sat-per-vbyte")
		confTarget, _ := cmd.Flags().GetUint32("conf-target")
		sweepAddress, _ := cmd.Flags().GetString("sweep-address")

		if feeRate < 0 {
			return fmt.Errorf("fee-rate-sat-per-vbyte must be " +
				"non-negative")
		}
		for _, outpoint := range outpoints {
			if err := validateOutpointString(outpoint); err != nil {
				return fmt.Errorf("invalid --outpoint %q: %w",
					outpoint, err)
			}
		}

		req.Outpoints = outpoints
		req.Broadcast = broadcast
		req.FeeRateSatPerVbyte = feeRate
		req.ConfTarget = confTarget
		req.SweepAddress = sweepAddress

		return nil
	}

	if err := parseRequest(cmd, req, fromFlags); err != nil {
		return err
	}

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	resp, err := client.SweepBoardingUTXOs(ctx, req)
	if err != nil {
		return fmt.Errorf("SweepBoardingUTXOs RPC failed: %w", err)
	}

	return printJSON(resp)
}

// sweepList executes the ListBoardingSweeps RPC and prints the result.
func sweepList(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	req := &waverpc.ListBoardingSweepsRequest{}
	if err := parseRequest(cmd, req, func() error {
		statusFilter, _ := cmd.Flags().GetString("status")
		pageSize, _ := cmd.Flags().GetUint32("page-size")
		pageToken, _ := cmd.Flags().GetString("page-token")

		req.Status = statusFilter
		req.PageSize = pageSize
		req.PageToken = pageToken

		return nil
	}); err != nil {
		return err
	}

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	resp, err := client.ListBoardingSweeps(ctx, req)
	if err != nil {
		return fmt.Errorf("ListBoardingSweeps RPC failed: %w", err)
	}

	items := make([]proto.Message, len(resp.Sweeps))
	for i, s := range resp.Sweeps {
		items[i] = s
	}

	return renderListOutput(cmd, resp, items)
}

// validateOutpointString validates the txid:index syntax accepted by sweep
// outpoint flags before making an RPC.
func validateOutpointString(outpoint string) error {
	parts := strings.SplitN(outpoint, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("expected txid:index")
	}
	if _, err := chainhash.NewHashFromStr(parts[0]); err != nil {
		return fmt.Errorf("invalid txid: %w", err)
	}
	if _, err := strconv.ParseUint(parts[1], 10, 32); err != nil {
		return fmt.Errorf("invalid index: %w", err)
	}

	return nil
}
