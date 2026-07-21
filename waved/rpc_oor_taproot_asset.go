package waved

import (
	"bytes"
	"errors"
	"strings"

	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// taprootAssetOORIntent maps and validates the optional experimental RPC
// extension without invoking tapd or acquiring a VTXO reservation.
func taprootAssetOORIntent(req *waverpc.SendOORRequest) (
	*oor.TaprootAssetOORIntent, error) {

	if req == nil || req.GetTaprootAsset() == nil {
		return nil, nil
	}

	rpcIntent := req.GetTaprootAsset()
	switch {
	case req.GetDryRun():
		return nil, status.Errorf(codes.InvalidArgument, "dry_run is "+
			"not supported for Taproot Asset OOR transfers")

	case len(req.GetRecipients()) != 1:
		return nil, status.Errorf(codes.InvalidArgument, "Taproot "+
			"Asset OOR transfers require exactly one recipient")

	case len(req.GetCustomInputs()) != 0:
		return nil, status.Errorf(codes.InvalidArgument, "Taproot "+
			"Asset OOR transfers do not support custom inputs")

	case strings.TrimSpace(req.GetIdempotencyKey()) == "":
		return nil, status.Errorf(codes.InvalidArgument, "Taproot "+
			"Asset OOR transfers require an idempotency key")

	case !rpcIntent.GetAcknowledgeUnconfirmed():
		return nil, status.Errorf(codes.InvalidArgument, "Taproot "+
			"Asset OOR transfers require "+
			"acknowledge_unconfirmed=true")
	}

	inputOutpoint, err := parseOutpointString(
		rpcIntent.GetInputVtxoOutpoint(),
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse "+
			"Taproot Asset input VTXO outpoint %q: %v",
			rpcIntent.GetInputVtxoOutpoint(), err)
	}

	intent := &oor.TaprootAssetOORIntent{
		InputVTXOOutpoint: inputOutpoint,
		AssetRef:          rpcIntent.GetAssetRef(),
		AssetAmount:       rpcIntent.GetAssetAmount(),
		ProofFile:         bytes.Clone(rpcIntent.GetInputProofFile()),
		RecipientScriptKey: bytes.Clone(
			rpcIntent.GetRecipientScriptKey(),
		),
		ProofCourierAddress: rpcIntent.GetProofCourierAddress(),
		ProofDeliveryMetadata: bytes.Clone(
			rpcIntent.GetProofDeliveryMetadata(),
		),
	}
	if err := intent.Validate(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	return intent, nil
}

// requireTaprootAssetOORPreparer fails before input reservation when the
// binary has no authenticated, restart-safe tap-sdk adapter installed.
func requireTaprootAssetOORPreparer(cfg *Config) (oor.TaprootAssetOORPreparer,
	error) {

	if cfg == nil || cfg.TaprootAssetOORPreparer == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "Taproot "+
			"Asset OOR preparer is not configured")
	}

	return cfg.TaprootAssetOORPreparer, nil
}

// taprootAssetOORPreparationError preserves a typed gRPC status from a
// concrete adapter and otherwise classifies the failure as internal backend
// work rather than malformed public input.
func taprootAssetOORPreparationError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, oor.ErrTaprootAssetCommitOutcomeUnknown) {
		return status.Errorf(codes.Aborted, "prepare Taproot Asset "+
			"OOR requires reconciliation: %v", err)
	}
	if status.Code(err) != codes.Unknown {
		return err
	}

	return status.Errorf(codes.Internal, "prepare Taproot Asset OOR: %v",
		err)
}

// validateTaprootAssetExactBTC keeps asset units and satoshis independent
// while requiring the first custom-anchor slice to avoid a Bitcoin change
// output that has no defined asset allocation.
func validateTaprootAssetExactBTC(inputTotal, targetAmount int64) error {
	if inputTotal == targetAmount {
		return nil
	}

	return status.Errorf(codes.InvalidArgument, "Taproot Asset OOR "+
		"transfer requires exact BTC value: input %d, recipient %d",
		inputTotal, targetAmount)
}

// invalidTaprootAssetPreparation reports a bug or compromised adapter result.
func invalidTaprootAssetPreparation(err error) error {
	return status.Errorf(codes.Internal, "invalid Taproot Asset OOR "+
		"preparation: %v", err)
}
