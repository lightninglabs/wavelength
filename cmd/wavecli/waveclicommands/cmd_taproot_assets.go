package waveclicommands

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
)

const taprootAssetOnboardingPollInterval = 5 * time.Second

type taprootAssetOnboardCall func(context.Context,
	*waverpc.OnboardTaprootAssetRequest) (
	*waverpc.OnboardTaprootAssetResponse, error)

// newTaprootAssetsCmd builds the prototype Taproot Asset command subtree.
func newTaprootAssetsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "taproot-assets",
		Short: "Prototype Taproot Asset operations",
		Long: "Prototype Taproot Asset operations backed by tapd " +
			"and tap-sdk inside waved. These commands are " +
			"intentionally advanced while the Wavelength " +
			"integration is evaluated.",
	}

	cmd.AddCommand(newTaprootAssetOnboardCmd())

	return cmd
}

// newTaprootAssetOnboardCmd creates the durable boarding-output command.
func newTaprootAssetOnboardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "onboard",
		Short: "Move one complete asset anchor into Wavelength",
		Long: "Move one complete, isolated, confirmed Taproot " +
			"Asset proof into a standard Wavelength VTXO policy. " +
			"The current prototype requires tapd and waved to " +
			"use the same LND wallet, which supplies any " +
			"additional Bitcoin needed for the visible carrier " +
			"value, miner fee, and wallet change. The output is " +
			"an ordinary on-chain output until a round boards " +
			"it; rerunning with the same idempotency key and " +
			"flags returns the same output rather than making " +
			"another.",
		Args: cobra.NoArgs,
		RunE: onboardTaprootAsset,
	}

	flags := cmd.Flags()
	flags.String(
		"idempotency-key", "",
		"stable caller-generated key reused for every retry",
	)
	flags.String("asset-ref", "",
		"tap-sdk asset ID or group reference")
	flags.Uint64(
		"asset-amount", 0,
		"complete asset amount held by the selected proof",
	)
	flags.String(
		"proof-file", "",
		"path to the complete confirmed Taproot Asset proof file",
	)
	flags.Uint64(
		"carrier-value-sat", 0, "Bitcoin value carried by the "+
			"asset VTXO (zero uses the operator minimum)",
	)
	flags.Uint64(
		"sat-per-vbyte", 0, "explicit on-chain fee rate (mutually "+
			"exclusive with --target-conf)",
	)
	flags.Uint32(
		"target-conf", 0, "on-chain confirmation target (mutually "+
			"exclusive with --sat-per-vbyte)",
	)
	flags.Uint64(
		"max-fee-sat", 0, "hard upper bound for the on-chain miner fee",
	)

	return cmd
}

// onboardTaprootAsset reads the proof bytes and invokes the durable daemon
// workflow.
func onboardTaprootAsset(cmd *cobra.Command, _ []string) error {
	request, err := taprootAssetOnboardingRequest(cmd)
	if err != nil {
		return invalidArgs(err)
	}

	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	response, err := client.OnboardTaprootAsset(cmd.Context(), request)
	if err != nil {
		return fmt.Errorf("OnboardTaprootAsset RPC failed: %w", err)
	}

	return printJSON(response)
}

// taprootAssetOnboardingRequest validates the prototype CLI contract and
// loads the proof file without changing its bytes between retries.
func taprootAssetOnboardingRequest(cmd *cobra.Command) (
	*waverpc.OnboardTaprootAssetRequest, error) {

	idempotencyKey, _ := cmd.Flags().GetString("idempotency-key")
	assetRef, _ := cmd.Flags().GetString("asset-ref")
	assetAmount, _ := cmd.Flags().GetUint64("asset-amount")
	proofPath, _ := cmd.Flags().GetString("proof-file")
	carrierValueSat, _ := cmd.Flags().GetUint64("carrier-value-sat")
	feeRateSatPerVByte, _ := cmd.Flags().GetUint64("sat-per-vbyte")
	targetConf, _ := cmd.Flags().GetUint32("target-conf")
	maxFeeSat, _ := cmd.Flags().GetUint64("max-fee-sat")

	switch {
	case idempotencyKey == "":
		return nil, fmt.Errorf("--idempotency-key is required")

	case assetRef == "":
		return nil, fmt.Errorf("--asset-ref is required")

	case assetAmount == 0:
		return nil, fmt.Errorf("--asset-amount must be positive")

	case proofPath == "":
		return nil, fmt.Errorf("--proof-file is required")

	case maxFeeSat == 0:
		return nil, fmt.Errorf("--max-fee-sat must be positive")

	case feeRateSatPerVByte == 0 && targetConf == 0:
		return nil, fmt.Errorf("exactly one of --sat-per-vbyte and " +
			"--target-conf is required")

	case feeRateSatPerVByte != 0 && targetConf != 0:
		return nil, fmt.Errorf("--sat-per-vbyte and --target-conf " +
			"are mutually exclusive")
	}

	if err := validateFreeText(
		"--idempotency-key", idempotencyKey,
	); err != nil {
		return nil, err
	}

	proofPath, err := expandCLIPath(proofPath)
	if err != nil {
		return nil, fmt.Errorf("expand --proof-file: %w", err)
	}
	// The command's purpose is to read the proof path selected by its
	// caller and forward those exact bytes to the local daemon.
	//nolint:gosec
	proofFile, err := os.ReadFile(proofPath)
	if err != nil {
		return nil, fmt.Errorf("read --proof-file %q: %w", proofPath,
			err)
	}
	if len(proofFile) == 0 {
		return nil, fmt.Errorf("--proof-file must not be empty")
	}

	return &waverpc.OnboardTaprootAssetRequest{
		IdempotencyKey:     idempotencyKey,
		AssetRef:           assetRef,
		AssetAmount:        assetAmount,
		InputProofFile:     proofFile,
		MaxFeeSat:          maxFeeSat,
		CarrierValueSat:    carrierValueSat,
		FeeRateSatPerVbyte: feeRateSatPerVByte,
		TargetConf:         targetConf,
	}, nil
}
