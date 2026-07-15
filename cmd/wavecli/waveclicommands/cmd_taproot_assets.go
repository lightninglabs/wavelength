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

// newTaprootAssetOnboardCmd creates the durable direct-deposit command. A
// caller reruns the same command until the response state becomes READY.
func newTaprootAssetOnboardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "onboard",
		Short: "Move one complete asset anchor into Wavelength",
		Long: "Move one complete, isolated, confirmed Taproot " +
			"Asset proof into a standard Wavelength VTXO policy. " +
			"The current prototype requires tapd and waved to " +
			"use the same LND wallet. Pass --wait, or preserve " +
			"every flag and rerun this command after " +
			"confirmation when the response state is pending.",
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
		"max-fee-sat", 0,
		"exact Bitcoin fee subtracted from the current anchor",
	)
	flags.Bool(
		"wait", false,
		"retry the same durable request until the anchor is ready",
	)

	return cmd
}

// onboardTaprootAsset reads the proof bytes and invokes the durable daemon
// workflow. A pending-confirmation response is successful and safe to retry.
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

	wait, _ := cmd.Flags().GetBool("wait")
	response, err := waitForTaprootAssetOnboarding(
		cmd.Context(), request, wait,
		taprootAssetOnboardingPollInterval,
		func(ctx context.Context,
			request *waverpc.OnboardTaprootAssetRequest) (
			*waverpc.OnboardTaprootAssetResponse, error) {

			return client.OnboardTaprootAsset(ctx, request)
		},
	)
	if err != nil {
		return fmt.Errorf("OnboardTaprootAsset RPC failed: %w", err)
	}

	return printJSON(response)
}

// waitForTaprootAssetOnboarding retries only pending-confirmation responses.
// Every attempt reuses the exact request object and therefore the same proof
// bytes and idempotency key.
func waitForTaprootAssetOnboarding(ctx context.Context,
	request *waverpc.OnboardTaprootAssetRequest, wait bool,
	pollInterval time.Duration,
	call taprootAssetOnboardCall) (*waverpc.OnboardTaprootAssetResponse,
	error) {

	for {
		response, err := call(ctx, request)
		if err != nil {
			return nil, err
		}
		if !wait || response.GetState() !=
			waverpc.TaprootAssetOnboardingState_TAPROOT_ASSET_ONBOARDING_STATE_PENDING_CONFIRMATION { //nolint:ll

			return response, nil
		}

		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}

			return nil, ctx.Err()

		case <-timer.C:
		}
	}
}

// taprootAssetOnboardingRequest validates the prototype CLI contract and
// loads the proof file without changing its bytes between retries.
func taprootAssetOnboardingRequest(cmd *cobra.Command) (
	*waverpc.OnboardTaprootAssetRequest, error) {

	idempotencyKey, _ := cmd.Flags().GetString("idempotency-key")
	assetRef, _ := cmd.Flags().GetString("asset-ref")
	assetAmount, _ := cmd.Flags().GetUint64("asset-amount")
	proofPath, _ := cmd.Flags().GetString("proof-file")
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
		IdempotencyKey: idempotencyKey,
		AssetRef:       assetRef,
		AssetAmount:    assetAmount,
		InputProofFile: proofFile,
		MaxFeeSat:      maxFeeSat,
	}, nil
}
