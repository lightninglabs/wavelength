package waveclicommands

import (
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/spf13/cobra"
)

// newActivityInspectCmd builds the wallet activity inspection subcommand.
func newActivityInspectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect <id>",
		Short: "Inspect one wallet activity entry",
		Long: "Inspect one wallet activity entry and show correlated " +
			"swap, VTXO, and ledger details.",
		Args: cobra.ExactArgs(1),
		RunE: inspectActivity,
	}

	cmd.Flags().Uint32("ledger-limit", 0,
		"maximum ledger rows to scan; 0 uses daemon maximum")
	cmd.Flags().String("format", "expanded",
		"output format (expanded|x|json)")

	return cmd
}

// inspectActivity calls the wallet inspection RPC and renders the selected
// output format.
func inspectActivity(cmd *cobra.Command, args []string) error {
	ledgerLimit, _ := cmd.Flags().GetUint32("ledger-limit")
	format, _ := cmd.Flags().GetString("format")
	if err := validateInspectFormat(format); err != nil {
		return err
	}

	req := &walletdkrpc.InspectActivityRequest{
		Id:          args[0],
		LedgerLimit: ledgerLimit,
	}

	return withWalletInspectionClient(
		cmd, func(c walletdkrpc.WalletInspectionServiceClient) error {
			resp, err := c.InspectActivity(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("activity inspect: %w", err)
			}

			switch format {
			case "json":
				return printWalletProto(resp)

			case "expanded", "x", "":
				return printWalletInspectionExpanded(resp)
			}

			return nil
		},
	)
}

// validateInspectFormat rejects unsupported activity inspection formats.
func validateInspectFormat(format string) error {
	switch format {
	case "", "json", "expanded", "x":
		return nil

	default:
		return fmt.Errorf("unknown inspect format %q: expected json, "+
			"expanded, or x", format)
	}
}
