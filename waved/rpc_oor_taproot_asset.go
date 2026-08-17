package waved

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/wire/v2"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
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

	// New asset-leaf carriers are operator-funded at the operator's
	// minimum VTXO amount, so no caller-chosen Bitcoin value survives.
	case rpcIntent.GetAssetChangeCarrierValueSat() != 0:
		return nil, status.Errorf(codes.InvalidArgument,
			"asset_change_carrier_value_sat must be zero: "+
				"carriers are operator-funded at the floor")

	case req.GetRecipients()[0].GetAmountSat() != 0:
		return nil, status.Errorf(codes.InvalidArgument, "recipient "+
			"amount_sat must be zero for Taproot Asset sends: "+
			"the daemon stamps the operator-funded carrier at "+
			"the operator's minimum VTXO amount")
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
		InputVTXOOutpoint:    inputOutpoint,
		AssetRef:             rpcIntent.GetAssetRef(),
		AssetAmount:          rpcIntent.GetAssetAmount(),
		RecipientAssetAmount: rpcIntent.GetRecipientAssetAmount(),
		AssetChangeCarrierValueSat: rpcIntent.
			GetAssetChangeCarrierValueSat(),
		ProofFile: bytes.Clone(
			rpcIntent.GetInputProofFile(),
		),
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

// resumeTaprootAssetOOR asks a durable concrete preparer whether this exact
// public request already owns a complete durable reservation set or crossed
// tapd's first commit boundary. Preparers that do not implement restart
// adoption retain the ordinary selection behavior.
func resumeTaprootAssetOOR(ctx context.Context,
	preparer oor.TaprootAssetOORPreparer,
	request *oor.TaprootAssetOORResumeRequest) (*oor.TaprootAssetOORResume,
	error) {

	resumer, ok := preparer.(oor.TaprootAssetOORPreparationResumer)
	if !ok {
		return nil, nil
	}
	resume, err := resumer.ResumeTaprootAssetOOR(ctx, request)
	if err != nil {
		return nil, taprootAssetOORPreparationError(err)
	}
	if resume == nil {
		return nil, nil
	}
	if len(resume.InputOutpoints) == 0 ||
		len(resume.InputOutpoints) >
			oortx.MaxTaprootAssetCheckpointPackages {
		return nil, invalidTaprootAssetPreparation(
			fmt.Errorf(
				"resume input count %d is invalid",
				len(resume.InputOutpoints),
			),
		)
	}

	seen := make(map[wire.OutPoint]struct{}, len(resume.InputOutpoints))
	assetMatches := 0
	for _, outpoint := range resume.InputOutpoints {
		if _, ok := seen[outpoint]; ok {
			return nil, invalidTaprootAssetPreparation(
				fmt.Errorf("resume contains duplicate "+
					"input %s", outpoint),
			)
		}
		seen[outpoint] = struct{}{}
		if outpoint == request.Intent.InputVTXOOutpoint {
			assetMatches++
		}
	}
	if assetMatches != 1 {
		return nil, invalidTaprootAssetPreparation(
			fmt.Errorf("resume does not contain the requested " +
				"asset input exactly once"),
		)
	}

	return resume, nil
}

// validateTaprootAssetOORResumeInputs binds preparer-side reservation
// ownership to the wallet's authoritative lifecycle state. Descriptor lookup
// alone is not adoption: a Live, spent, or exiting VTXO must never reach
// resumed tapd work even if a stale journal names its outpoint.
func validateTaprootAssetOORResumeInputs(ctx context.Context,
	store vtxo.VTXOStore, resume *oor.TaprootAssetOORResume) error {

	if store == nil || resume == nil {
		return invalidTaprootAssetPreparation(
			fmt.Errorf("resume VTXO store and inputs are required"),
		)
	}
	for _, outpoint := range resume.InputOutpoints {
		descriptor, err := store.GetVTXO(ctx, outpoint)
		if err != nil {
			return invalidTaprootAssetPreparation(
				fmt.Errorf("load resumed input %s: %w",
					outpoint, err),
			)
		}
		if descriptor == nil || descriptor.Outpoint != outpoint {
			return invalidTaprootAssetPreparation(
				fmt.Errorf("resumed input %s descriptor "+
					"mismatch", outpoint),
			)
		}
		if descriptor.Status != vtxo.VTXOStatusSpending {
			return taprootAssetOORPreparationError(
				fmt.Errorf("%w: resumed input %s has "+
					"status %s",
					oor.ErrTaprootAssetCommitOutcomeUnknown,
					outpoint, descriptor.Status),
			)
		}
	}

	return nil
}

// invalidTaprootAssetPreparation reports a bug or compromised adapter result.
func invalidTaprootAssetPreparation(err error) error {
	return status.Errorf(codes.Internal, "invalid Taproot Asset OOR "+
		"preparation: %v", err)
}

// registerTaprootAssetChangeAliases persists composed-script ownership for the
// wallet's own change outputs before the transfer is admitted. The incoming
// self-notification for this session is driven by a single recipient event,
// so a composed asset-change script must resolve as owned without relying on
// its own event's metadata overlay.
func (r *RPCServer) registerTaprootAssetChangeAliases(ctx context.Context,
	preparation *oor.TaprootAssetOORPreparation,
	lease *oor.OORCarrierLease) error {

	store, err := r.newOORReceiveScriptStore()
	if err != nil {
		return fmt.Errorf("open OOR receive-script store: %w", err)
	}

	for i := range preparation.Recipients {
		recipient := preparation.Recipients[i]
		if recipient.Value == preparation.Receiver.Value &&
			bytes.Equal(
				recipient.PkScript,
				preparation.Receiver.PkScript,
			) {

			continue
		}

		// The operator's float residual is not wallet money and has
		// no owned receive script to alias.
		if lease != nil &&
			bytes.Equal(recipient.PkScript, lease.PkScript) {

			continue
		}

		_, err := ResolveOwnedReceiveScriptKey(
			ctx, store, oor.ArkRecipientOutput{
				Value:    recipient.Value,
				PkScript: recipient.PkScript,
				VTXOPolicyTemplate: recipient.
					VTXOPolicyTemplate,
				TaprootAssetRoot: recipient.TaprootAssetRoot,
				TaprootAssetRef:  recipient.TaprootAssetRef,
				TaprootAssetAmount: recipient.
					TaprootAssetAmount,
			},
		)
		if err != nil {
			return fmt.Errorf("resolve local change recipient "+
				"%d: %w", i, err)
		}
	}

	return nil
}
