package waveclicommands

import (
	"fmt"
	"os"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/spf13/cobra"
)

// newExitExportCmd exports signed ancestry and an optional anchored sweep.
// File output is exclusive to avoid overwriting an unrelated recovery bundle.
func newExitExportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export signed ancestry for externally funded unrolling",
		Long: "Exports verified signed ancestry as JSON for an " +
			"external broadcaster that supplies its own CPFP fee " +
			"inputs. Export does not start an exit or reserve " +
			"the VTXO. Add --sweep-address to include a signed " +
			"final sweep with a service-funded fee anchor. " +
			"Sharing it allows its recipient to initiate " +
			"unrolling and reveals the ancestry.\n\n" +
			"Example:\n" +
			"  wavecli exit export --outpoint TXID:VOUT " +
			"--output ancestry.json",
		Args: cobra.NoArgs,
		RunE: walletExitExport,
	}
	cmd.Flags().String("outpoint", "", "VTXO outpoint (txid:vout)")
	_ = cmd.MarkFlagRequired("outpoint")
	cmd.Flags().String("sweep-address", "",
		"include a presigned final sweep to this Bitcoin address")
	cmd.Flags().String("output", "",
		"new bundle file; omitted writes JSON to stdout")

	return cmd
}

// walletExitExport fetches ancestry without the local unroll admission path.
func walletExitExport(cmd *cobra.Command, _ []string) error {
	outpoint, _ := cmd.Flags().GetString("outpoint")
	if err := invalidArgs(validateOutpoint(outpoint)); err != nil {
		return err
	}
	output, _ := cmd.Flags().GetString("output")
	sweepAddress, _ := cmd.Flags().GetString("sweep-address")
	if sweepAddress != "" {
		if err := invalidArgs(
			validateDestination(sweepAddress),
		); err != nil {
			return err
		}
	}

	return withWalletClient(
		cmd, func(client wavewalletrpc.WalletServiceClient) error {
			ctx, cancel := rpcContext(cmd)
			defer cancel()

			response, err := client.ExportExit(
				ctx, &wavewalletrpc.ExportExitRequest{
					Outpoint:     outpoint,
					SweepAddress: sweepAddress,
				},
			)
			if err != nil {
				return fmt.Errorf("exit export: %w", err)
			}

			return writeExitExport(cmd, output, response)
		},
	)
}

// writeExitExport emits the portable JSON response to stdout or a private new
// file. Existing files and symlinks are refused, and partial files are removed.
func writeExitExport(cmd *cobra.Command, output string,
	response *wavewalletrpc.ExportExitResponse) error {

	data, err := walletProtoMarshal.Marshal(response)
	if err != nil {
		return fmt.Errorf("marshal exit export: %w", err)
	}
	data = append(data, '\n')
	if output == "" {
		_, err := cmd.OutOrStdout().Write(data)

		return err
	}

	// The user explicitly chooses a new file; O_EXCL refuses symlinks.
	//nolint:gosec
	file, err := os.OpenFile(
		output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600,
	)
	if err != nil {
		return fmt.Errorf("create exit export: %w", err)
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(output)
		if writeErr != nil {
			return fmt.Errorf("write exit export: %w", writeErr)
		}

		return fmt.Errorf("close exit export: %w", closeErr)
	}

	return nil
}
