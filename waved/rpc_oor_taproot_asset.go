package waved

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
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

	inputOutpoints, err := taprootAssetIntentOutpoints(rpcIntent)
	if err != nil {
		return nil, err
	}

	intent := &oor.TaprootAssetOORIntent{
		InputVTXOOutpoints:   inputOutpoints,
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

// taprootAssetIntentOutpoints resolves the pinned input, if any. An empty
// outpoint delegates input selection to the daemon, where asset_amount
// already names the units to send: the split between recipient and change
// derives from whatever selection covers it.
func taprootAssetIntentOutpoints(rpcIntent *waverpc.TaprootAssetOORIntent) (
	[]wire.OutPoint, error) {

	rawOutpoint := rpcIntent.GetInputVtxoOutpoint()
	if rawOutpoint == "" {
		if rpcIntent.GetRecipientAssetAmount() != 0 {
			message := "recipient_asset_amount requires " +
				"input_vtxo_outpoint: asset_amount already " +
				"names the units to send"

			return nil, status.Error(
				codes.InvalidArgument, message,
			)
		}
		if len(rpcIntent.GetInputProofFile()) != 0 {
			return nil, status.Errorf(codes.InvalidArgument,
				"input_proof_file requires input_vtxo_outpoint")
		}

		return nil, nil
	}

	inputOutpoint, err := parseOutpointString(rawOutpoint)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse "+
			"Taproot Asset input VTXO outpoint %q: %v", rawOutpoint,
			err)
	}

	return []wire.OutPoint{inputOutpoint}, nil
}

// selectTaprootAssetOORInputs picks the live asset VTXOs one atomic transfer
// spends to cover want units. A single sufficient VTXO wins (the smallest
// such), otherwise the largest VTXOs accumulate until covered, spine first,
// bounded by the per-transfer input cap.
func selectTaprootAssetOORInputs(candidates []*vtxo.Descriptor,
	want uint64) ([]*vtxo.Descriptor, error) {

	if want == 0 {
		return nil, fmt.Errorf("taproot asset send amount is required")
	}

	live := make([]*vtxo.Descriptor, 0, len(candidates))
	var total uint64
	for _, candidate := range candidates {
		if candidate == nil ||
			candidate.Status != vtxo.VTXOStatusLive ||
			candidate.TaprootAssetAmount == 0 {

			continue
		}
		if candidate.TaprootAssetAmount > ^uint64(0)-total {
			return nil, fmt.Errorf("taproot asset balance " +
				"overflows")
		}
		total += candidate.TaprootAssetAmount
		live = append(live, candidate)
	}
	if total < want {
		return nil, fmt.Errorf("insufficient taproot asset balance: "+
			"need %d units, have %d", want, total)
	}

	// Deterministic order: units descending, outpoint as tie-break.
	sort.Slice(live, func(i, j int) bool {
		if live[i].TaprootAssetAmount != live[j].TaprootAssetAmount {
			return live[i].TaprootAssetAmount >
				live[j].TaprootAssetAmount
		}

		return live[i].Outpoint.String() < live[j].Outpoint.String()
	})

	// A single sufficient VTXO avoids merging entirely: the smallest one
	// covering the send preserves the larger leaves. Equal amounts keep
	// the earlier entry so the outpoint tie-break stays in force.
	var single *vtxo.Descriptor
	for _, candidate := range live {
		if candidate.TaprootAssetAmount < want {
			break
		}
		if single == nil ||
			candidate.TaprootAssetAmount <
				single.TaprootAssetAmount {

			single = candidate
		}
	}
	if single != nil {
		return []*vtxo.Descriptor{single}, nil
	}

	var (
		selected []*vtxo.Descriptor
		covered  uint64
	)
	for _, candidate := range live {
		if len(selected) == oor.MaxTaprootAssetInputs {
			return nil, fmt.Errorf("sending %d units spans more "+
				"than %d asset VTXOs; consolidate first", want,
				oor.MaxTaprootAssetInputs)
		}
		selected = append(selected, candidate)
		covered += candidate.TaprootAssetAmount
		if covered >= want {
			return selected, nil
		}
	}

	return nil, fmt.Errorf("sending %d units spans more than %d asset "+
		"VTXOs; consolidate first", want, oor.MaxTaprootAssetInputs)
}

// resolveTaprootAssetOORIntent turns the public intent into the resolved
// per-input shape: the ordered asset input set (adopted from a resumed
// preparation, pinned by the caller, or selected from the live wallet), the
// summed input units, and the derived recipient allocation. The second
// return value is the summed Bitcoin carrier value wallet selection targets.
func (r *RPCServer) resolveTaprootAssetOORIntent(ctx context.Context,
	intent *oor.TaprootAssetOORIntent, resume *oor.TaprootAssetOORResume) (
	*oor.TaprootAssetOORIntent, btcutil.Amount, error) {

	pinned := len(intent.InputVTXOOutpoints) != 0

	var outpoints []wire.OutPoint
	switch {
	case resume != nil:
		outpoints = append(
			[]wire.OutPoint(nil), resume.InputOutpoints...,
		)

	case pinned:
		outpoints = append(
			[]wire.OutPoint(nil), intent.InputVTXOOutpoints...,
		)

	default:
		live, err := r.server.vtxoStore.ListLiveVTXOs(ctx)
		if err != nil {
			return nil, 0, status.Errorf(codes.Internal, "list "+
				"live VTXOs: %v", err)
		}

		// Exact reference match only: the resolved inputs must carry
		// the intent's reference verbatim through preparation.
		candidates := make([]*vtxo.Descriptor, 0, len(live))
		for _, desc := range live {
			if desc.TaprootAssetRef == intent.AssetRef {
				candidates = append(candidates, desc)
			}
		}
		selected, err := selectTaprootAssetOORInputs(
			candidates, intent.AssetAmount,
		)
		if err != nil {
			return nil, 0, status.Errorf(codes.FailedPrecondition,
				"%v", err)
		}
		for _, desc := range selected {
			outpoints = append(outpoints, desc.Outpoint)
		}
	}

	var (
		totalUnits uint64
		carriers   btcutil.Amount
	)
	for _, outpoint := range outpoints {
		desc, err := r.server.vtxoStore.GetVTXO(ctx, outpoint)
		if err != nil || desc == nil {
			return nil, 0, status.Errorf(codes.InvalidArgument,
				"unknown Taproot Asset input VTXO %s", outpoint)
		}
		totalUnits += desc.TaprootAssetAmount
		carriers += desc.Amount
	}

	resolved := *intent
	resolved.InputVTXOOutpoints = outpoints
	if !pinned {
		// Selection mode: the caller's asset_amount is the send
		// total; the resolved total is whatever the inputs carry.
		want := intent.AssetAmount
		if want > totalUnits {
			return nil, 0, status.Errorf(codes.FailedPrecondition,
				"resolved asset inputs carry %d units, need %d",
				totalUnits, want)
		}
		resolved.AssetAmount = totalUnits
		resolved.RecipientAssetAmount = 0
		if want < totalUnits {
			resolved.RecipientAssetAmount = want
		}
	}
	if err := resolved.Validate(); err != nil {
		return nil, 0, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	return &resolved, carriers, nil
}

// orderAssetSelectedOutpoints pins wallet-locked outpoints to the intent's
// spine-first order. The wallet must have selected exactly the required set:
// asset sends never carry extra Bitcoin filler inputs.
func orderAssetSelectedOutpoints(selected, required []wire.OutPoint) (
	[]wire.OutPoint, error) {

	if len(selected) != len(required) {
		return nil, fmt.Errorf("wallet selected %d inputs, want %d",
			len(selected), len(required))
	}
	seen := make(map[wire.OutPoint]struct{}, len(selected))
	for _, outpoint := range selected {
		seen[outpoint] = struct{}{}
	}
	for _, outpoint := range required {
		if _, ok := seen[outpoint]; !ok {
			return nil, fmt.Errorf("wallet selection misses "+
				"required input %s", outpoint)
		}
	}

	return append([]wire.OutPoint(nil), required...), nil
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
		len(resume.InputOutpoints) > oor.MaxTaprootAssetInputs {
		return nil, invalidTaprootAssetPreparation(
			fmt.Errorf(
				"resume input count %d is invalid",
				len(resume.InputOutpoints),
			),
		)
	}

	seen := make(map[wire.OutPoint]struct{}, len(resume.InputOutpoints))
	for _, outpoint := range resume.InputOutpoints {
		if _, ok := seen[outpoint]; ok {
			return nil, invalidTaprootAssetPreparation(
				fmt.Errorf("resume contains duplicate "+
					"input %s", outpoint),
			)
		}
		seen[outpoint] = struct{}{}
	}

	// Caller-pinned outpoints must be adopted verbatim: the journaled set
	// has to be exactly the pinned set. A selection-mode retry pins
	// nothing and adopts whatever selection the journal committed to.
	pinned := request.Intent.InputVTXOOutpoints
	if len(pinned) != 0 {
		if len(resume.InputOutpoints) != len(pinned) {
			return nil, invalidTaprootAssetPreparation(
				fmt.Errorf(
					"resume input count %d does not "+
						"match the %d pinned asset "+
						"inputs",
					len(resume.InputOutpoints), len(pinned),
				),
			)
		}
		for _, outpoint := range pinned {
			if _, ok := seen[outpoint]; !ok {
				return nil, invalidTaprootAssetPreparation(
					fmt.Errorf("resume does not contain "+
						"the requested asset input "+
						"%s exactly once", outpoint),
				)
			}
		}
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
