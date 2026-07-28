package oor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
)

// ErrTaprootAssetCommitOutcomeUnknown reports an asset commit attempt whose
// durable outcome cannot be established. Callers must retain every input
// reservation and reconcile the tapd transition before retrying or releasing
// the inputs.
var ErrTaprootAssetCommitOutcomeUnknown = errors.New("taproot asset commit " +
	"outcome is unknown")

const (
	// MaxTaprootAssetRefBytes bounds the opaque tap-sdk asset identifier at
	// the daemon boundary.
	MaxTaprootAssetRefBytes = oortx.MaxTaprootAssetRefBytes

	// MaxTaprootAssetProofDeliveryBytes bounds host-owned receiver
	// metadata.
	MaxTaprootAssetProofDeliveryBytes = 1024 * 1024

	// MaxTaprootAssetCourierAddressBytes bounds the optional proof courier
	// address before the concrete tap-sdk adapter interprets it.
	MaxTaprootAssetCourierAddressBytes = 2048
)

// TaprootAssetOORIntent is the proof-selected asset movement requested by an
// OOR caller. AssetAmount is measured in asset units and is deliberately
// separate from the satoshi value of the containing Ark VTXO.
type TaprootAssetOORIntent struct {
	// InputVTXOOutpoint is the wallet-managed asset-bearing VTXO that must
	// be selected and reserved through the normal VTXO manager path.
	InputVTXOOutpoint wire.OutPoint

	// AssetRef is the opaque tap-sdk asset or group identifier.
	AssetRef string

	// AssetAmount is the exact number of asset units selected by ProofFile.
	AssetAmount uint64

	// RecipientAssetAmount is the number of asset units delivered to the
	// caller's receiver. Zero preserves the legacy full-send default.
	RecipientAssetAmount uint64

	// AssetChangeCarrierValueSat is the explicit Bitcoin value assigned to
	// local asset change. It must be zero for a full asset send.
	AssetChangeCarrierValueSat uint64

	// ProofFile is the complete confirmed proof for the selected asset.
	ProofFile []byte

	// RecipientScriptKey is a deprecated wire field and must be empty. Ark
	// policy ownership requires the preparer to derive an OP_TRUE asset
	// script instead of honoring a second recipient ownership key.
	RecipientScriptKey []byte

	// ProofCourierAddress optionally identifies the receiver proof courier.
	ProofCourierAddress string

	// ProofDeliveryMetadata is opaque receiver-owned delivery metadata.
	ProofDeliveryMetadata []byte
}

// Validate checks cheap, SDK-neutral bounds before any tapd call can occur.
// The concrete tap-sdk adapter remains responsible for parsing AssetRef and
// proving that ProofFile selects exactly AssetAmount units.
func (i *TaprootAssetOORIntent) Validate() error {
	if i == nil {
		return fmt.Errorf("taproot asset OOR intent must be provided")
	}

	assetRef := strings.TrimSpace(i.AssetRef)
	if assetRef == "" {
		return fmt.Errorf("taproot asset ref is required")
	}
	if len(assetRef) > MaxTaprootAssetRefBytes {
		return fmt.Errorf("taproot asset ref exceeds %d bytes",
			MaxTaprootAssetRefBytes)
	}
	if i.AssetAmount == 0 {
		return fmt.Errorf("taproot asset amount is required")
	}
	if len(i.ProofFile) > oortx.MaxTaprootAssetPackageBytes {
		return fmt.Errorf("taproot asset input proof exceeds %d bytes",
			oortx.MaxTaprootAssetPackageBytes)
	}
	if len(i.RecipientScriptKey) != 0 {
		return fmt.Errorf("taproot asset recipient script key is " +
			"deprecated and must be empty")
	}
	recipientAmount := i.EffectiveRecipientAssetAmount()
	if recipientAmount > i.AssetAmount {
		return fmt.Errorf("taproot asset recipient amount exceeds " +
			"input amount")
	}
	if recipientAmount == i.AssetAmount &&
		i.AssetChangeCarrierValueSat != 0 {
		return fmt.Errorf("taproot asset change carrier must be zero " +
			"for a full send")
	}
	if recipientAmount < i.AssetAmount &&
		i.AssetChangeCarrierValueSat == 0 {
		return fmt.Errorf("taproot asset change carrier is required " +
			"for a partial send")
	}
	if i.AssetChangeCarrierValueSat > uint64(btcutil.MaxSatoshi) {
		return fmt.Errorf("taproot asset change carrier exceeds " +
			"maximum satoshi value")
	}
	if len(i.ProofCourierAddress) >
		MaxTaprootAssetCourierAddressBytes {
		return fmt.Errorf("taproot asset proof courier address "+
			"exceeds %d bytes", MaxTaprootAssetCourierAddressBytes)
	}
	if i.ProofCourierAddress != "" {
		if _, err := url.ParseRequestURI(
			i.ProofCourierAddress,
		); err != nil {
			return fmt.Errorf("taproot asset proof courier "+
				"address is invalid: %w", err)
		}
	}
	if len(i.ProofDeliveryMetadata) >
		MaxTaprootAssetProofDeliveryBytes {
		return fmt.Errorf("taproot asset proof delivery metadata "+
			"exceeds %d bytes", MaxTaprootAssetProofDeliveryBytes)
	}

	return nil
}

// EffectiveRecipientAssetAmount returns the explicit receiver allocation or
// the legacy full-send default when the additive field is omitted.
func (i *TaprootAssetOORIntent) EffectiveRecipientAssetAmount() uint64 {
	if i == nil || i.RecipientAssetAmount == 0 {
		if i == nil {
			return 0
		}

		return i.AssetAmount
	}

	return i.RecipientAssetAmount
}

// TaprootAssetChangeRecipientBuilder derives a wallet-owned Ark policy output
// for either asset change or ordinary Bitcoin change.
type TaprootAssetChangeRecipientBuilder func(context.Context,
	btcutil.Amount) (oortx.RecipientOutput, error)

// TaprootAssetOORPrepareRequest is the immutable host graph handed to the
// custom-anchor orchestration boundary before any Bitcoin signature exists.
type TaprootAssetOORPrepareRequest struct {
	// RequestID is the caller's durable idempotency key. Implementations
	// must reconcile repeated IDs with an earlier tapd outcome instead of
	// blindly committing a second asset transition.
	RequestID string

	// Policy is the operator checkpoint policy for this OOR session.
	Policy arkscript.CheckpointPolicy

	// Inputs are the exact asset-bearing VTXOs selected by the caller.
	Inputs []TransferInput

	// Recipients are the Bitcoin-only policy outputs before asset roots are
	// composed into their Taproot output keys. The caller supplies exactly
	// one receiver; the preparer derives local change before its first tapd
	// commit.
	Recipients []oortx.RecipientOutput

	// OutputFloor is the operator's minimum VTXO carrier value.
	OutputFloor btcutil.Amount

	// BuildChangeRecipient derives and registers wallet-owned change policy
	// outputs. The preparer invokes it only before the first external
	// commit and persists the exact result for restart.
	BuildChangeRecipient TaprootAssetChangeRecipientBuilder

	// Intent identifies the selected asset and its final receiver script.
	Intent TaprootAssetOORIntent
}

// Validate checks the first showcase contract before preparation begins.
func (r *TaprootAssetOORPrepareRequest) Validate() error {
	if r == nil {
		return fmt.Errorf("taproot asset OOR prepare request is " +
			"required")
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("taproot asset OOR request ID is required")
	}
	if r.Policy.OperatorKey == nil {
		return fmt.Errorf("taproot asset OOR operator key is required")
	}
	if len(r.Inputs) == 0 {
		return fmt.Errorf("taproot asset OOR requires at least one " +
			"input")
	}
	if len(r.Inputs) > oortx.MaxTaprootAssetCheckpointPackages {
		return fmt.Errorf("taproot asset OOR input count %d exceeds %d",
			len(r.Inputs), oortx.MaxTaprootAssetCheckpointPackages)
	}
	if len(r.Recipients) != 1 {
		return fmt.Errorf("taproot asset OOR requires exactly one " +
			"recipient")
	}
	if err := r.Intent.Validate(); err != nil {
		return err
	}
	if r.OutputFloor <= 0 {
		return fmt.Errorf("taproot asset OOR output floor is required")
	}
	assetInputIndex, err := r.AssetInputIndex()
	if err != nil {
		return err
	}
	assetInput := r.Inputs[assetInputIndex]
	if assetInput.VTXO.Outpoint != r.Intent.InputVTXOOutpoint {
		return fmt.Errorf("taproot asset OOR input outpoint does not " +
			"match the requested managed VTXO")
	}
	if assetInput.VTXO.TaprootAssetRef != r.Intent.AssetRef {
		return fmt.Errorf("taproot asset OOR input ref does not " +
			"match the requested asset")
	}
	if assetInput.VTXO.TaprootAssetAmount != r.Intent.AssetAmount {
		return fmt.Errorf("taproot asset OOR input amount does not " +
			"match the requested asset")
	}
	if len(r.Recipients[0].VTXOPolicyTemplate) == 0 {
		return fmt.Errorf("taproot asset OOR recipient policy is " +
			"required")
	}
	if r.Recipients[0].TaprootAssetRoot != nil ||
		r.Recipients[0].TaprootAssetRef != "" ||
		r.Recipients[0].TaprootAssetAmount != 0 {
		return fmt.Errorf("taproot asset OOR recipient must be " +
			"uncomposed")
	}

	if r.Recipients[0].Value < r.OutputFloor {
		return fmt.Errorf("taproot asset OOR receiver carrier is " +
			"below the output floor")
	}

	assetChange := btcutil.Amount(r.Intent.AssetChangeCarrierValueSat)
	if assetChange != 0 && assetChange < r.OutputFloor {
		return fmt.Errorf("taproot asset OOR change carrier is below " +
			"the output floor")
	}

	inputTotal, err := taprootAssetInputTotal(r.Inputs)
	if err != nil {
		return err
	}
	required, err := addTaprootAssetCarrier(
		r.Recipients[0].Value, assetChange,
	)
	if err != nil {
		return err
	}
	if required > inputTotal {
		return fmt.Errorf("taproot asset OOR carrier funding is " +
			"insufficient")
	}
	change := inputTotal - required
	if change != 0 && change < r.OutputFloor {
		return fmt.Errorf("taproot asset OOR Bitcoin change is below " +
			"the output floor")
	}
	if (assetChange != 0 || change != 0) &&
		r.BuildChangeRecipient == nil {
		return fmt.Errorf("taproot asset OOR change recipient " +
			"builder is required")
	}

	return nil
}

// AssetInputIndex returns the unique asset-bearing input. Every other input
// must be an ordinary Bitcoin VTXO.
func (r *TaprootAssetOORPrepareRequest) AssetInputIndex() (int, error) {
	if r == nil {
		return 0, fmt.Errorf("taproot asset OOR prepare request is " +
			"required")
	}

	assetIndex := -1
	seenOutpoints := make(map[wire.OutPoint]struct{}, len(r.Inputs))
	for idx := range r.Inputs {
		input := &r.Inputs[idx]
		if err := input.Validate(); err != nil {
			return 0, fmt.Errorf("taproot asset OOR input %d: %w",
				idx, err)
		}
		if _, ok := seenOutpoints[input.VTXO.Outpoint]; ok {
			return 0, fmt.Errorf("taproot asset OOR input %d has "+
				"duplicate outpoint %s", idx,
				input.VTXO.Outpoint)
		}
		seenOutpoints[input.VTXO.Outpoint] = struct{}{}

		hasAsset := input.TaprootAssetRoot != nil ||
			input.VTXO.TaprootAssetRoot != nil ||
			input.VTXO.TaprootAssetRef != "" ||
			input.VTXO.TaprootAssetAmount != 0
		if !hasAsset {
			continue
		}
		if input.TaprootAssetRoot == nil ||
			input.VTXO.TaprootAssetRoot == nil ||
			*input.TaprootAssetRoot !=
				*input.VTXO.TaprootAssetRoot ||
			input.VTXO.TaprootAssetRef == "" ||
			input.VTXO.TaprootAssetAmount == 0 {
			return 0, fmt.Errorf("taproot asset OOR input %d has "+
				"incomplete asset state", idx)
		}
		if assetIndex >= 0 {
			return 0, fmt.Errorf("taproot asset OOR requires " +
				"exactly one asset input")
		}
		assetIndex = idx
	}
	if assetIndex < 0 {
		return 0, fmt.Errorf("taproot asset OOR input root is required")
	}

	return assetIndex, nil
}

func taprootAssetInputTotal(inputs []TransferInput) (btcutil.Amount, error) {
	var total btcutil.Amount
	for idx := range inputs {
		amount := inputs[idx].VTXO.Amount
		if amount <= 0 || total > btcutil.MaxSatoshi-amount {
			return 0, fmt.Errorf("taproot asset OOR input " +
				"carrier sum overflows")
		}
		total += amount
	}

	return total, nil
}

func addTaprootAssetCarrier(left, right btcutil.Amount) (btcutil.Amount,
	error) {

	if left < 0 || right < 0 || left > btcutil.MaxSatoshi-right {
		return 0, fmt.Errorf("taproot asset OOR carrier sum overflows")
	}

	return left + right, nil
}

// TaprootAssetOORPreparation is the immutable custom-anchor result supplied
// to the durable OOR actor.
type TaprootAssetOORPreparation struct {
	// PreparedSubmit contains the committed Bitcoin graph and sealed
	// tap-sdk recovery packages.
	PreparedSubmit *PreparedSubmitPackage

	// Recipients contains the original recipients with output scripts and
	// Taproot Asset roots updated to match the committed Ark transaction.
	Recipients []oortx.RecipientOutput

	// Receiver is the caller-requested output after its asset root is
	// composed. Local asset and Bitcoin change are deliberately excluded.
	Receiver oortx.RecipientOutput
}

// Validate binds a preparation to the exact request that produced it.
func (p *TaprootAssetOORPreparation) Validate(
	request *TaprootAssetOORPrepareRequest) error {

	if p == nil {
		return fmt.Errorf("taproot asset OOR preparation is required")
	}
	if err := request.Validate(); err != nil {
		return err
	}
	if len(p.Recipients) < len(request.Recipients) {
		return fmt.Errorf("taproot asset OOR recipient count decreased")
	}
	for idx := range request.Recipients {
		before := request.Recipients[idx]
		after := p.Receiver
		if after.Value != before.Value {
			return fmt.Errorf("taproot asset OOR recipient %d "+
				"value changed", idx)
		}
		if !bytes.Equal(
			after.VTXOPolicyTemplate, before.VTXOPolicyTemplate,
		) {
			return fmt.Errorf("taproot asset OOR recipient %d "+
				"policy changed", idx)
		}
		if after.TaprootAssetRoot == nil {
			return fmt.Errorf("taproot asset OOR recipient %d "+
				"root is required", idx)
		}
		if err := after.ValidateTaprootAssetCommitment(); err != nil {
			return fmt.Errorf("taproot asset OOR recipient %d: %w",
				idx, err)
		}
	}
	if p.Receiver.TaprootAssetRef != request.Intent.AssetRef ||
		p.Receiver.TaprootAssetAmount !=
			request.Intent.EffectiveRecipientAssetAmount() {
		return fmt.Errorf("taproot asset OOR receiver allocation " +
			"changed")
	}

	var (
		assetTotal    uint64
		matchingCount int
	)
	for idx := range p.Recipients {
		recipient := p.Recipients[idx]
		if recipient.Value == p.Receiver.Value &&
			bytes.Equal(recipient.PkScript, p.Receiver.PkScript) {

			matchingCount++
		}
		if recipient.TaprootAssetRoot == nil {
			err := recipient.ValidateTaprootAssetMetadata()
			if err != nil {
				return fmt.Errorf("taproot asset OOR "+
					"recipient %d: %w", idx, err)
			}

			continue
		}
		err := recipient.ValidateTaprootAssetCommitment()
		if err != nil {
			return fmt.Errorf("taproot asset OOR recipient %d: %w",
				idx, err)
		}
		if recipient.TaprootAssetRef != request.Intent.AssetRef ||
			assetTotal > ^uint64(0)-recipient.TaprootAssetAmount {
			return fmt.Errorf("taproot asset OOR recipient " +
				"allocation mismatch")
		}
		assetTotal += recipient.TaprootAssetAmount
	}
	if matchingCount != 1 {
		return fmt.Errorf("taproot asset OOR receiver is not unique")
	}
	if assetTotal != request.Intent.AssetAmount {
		return fmt.Errorf("taproot asset OOR asset allocation is not " +
			"conserved")
	}
	if err := p.PreparedSubmit.Validate(
		request.Inputs, p.Recipients,
	); err != nil {
		return fmt.Errorf("taproot asset OOR prepared submit: %w", err)
	}

	return nil
}

// TaprootAssetOORPreparer commits both custom-anchor transitions before the
// durable OOR actor asks for Bitcoin signatures. PrepareTaprootAssetOOR must be
// restart-safe and idempotent by RequestID because a tapd commit response can
// be lost after tapd has already persisted the transition.
type TaprootAssetOORPreparer interface {
	PrepareTaprootAssetOOR(context.Context,
		*TaprootAssetOORPrepareRequest) (
		*TaprootAssetOORPreparation,
		error,
	)
}

// TaprootAssetOORResumeRequest identifies the selection-independent part of a
// public asset send. It lets the daemon recover the exact carrier inputs from a
// preparation that crossed tapd's first commit boundary without asking the
// wallet to select already-Spending VTXOs again.
type TaprootAssetOORResumeRequest struct {
	RequestID   string
	Policy      arkscript.CheckpointPolicy
	Recipients  []oortx.RecipientOutput
	OutputFloor btcutil.Amount
	Intent      TaprootAssetOORIntent
}

// TaprootAssetOORResume contains the exact input outpoints journaled before
// the first external asset commit.
type TaprootAssetOORResume struct {
	InputOutpoints []wire.OutPoint
}

// TaprootAssetOORPreparationResumer is an optional restart bridge implemented
// by durable preparers. A nil result means no side-effectful preparation needs
// adoption and ordinary wallet selection should proceed.
type TaprootAssetOORPreparationResumer interface {
	ResumeTaprootAssetOOR(context.Context,
		*TaprootAssetOORResumeRequest) (*TaprootAssetOORResume, error)
}
