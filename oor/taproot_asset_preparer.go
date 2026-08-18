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

	// AssetChangeCarrierValueSat is a deprecated wire field and must be
	// zero: new asset-leaf carriers are operator-funded at the operator's
	// minimum VTXO amount.
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
	if i.AssetChangeCarrierValueSat != 0 {
		return fmt.Errorf("taproot asset change carrier must be " +
			"zero: carriers are operator-funded at the " +
			"operator's minimum VTXO amount")
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

// NewAssetLeafCount returns the number of new asset-leaf carriers the send
// creates: the recipient leaf, plus an asset-change leaf on a partial send.
func (i *TaprootAssetOORIntent) NewAssetLeafCount() int {
	if i == nil {
		return 0
	}

	if i.EffectiveRecipientAssetAmount() < i.AssetAmount {
		return 2
	}

	return 1
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

	// Lease is the operator carrier-float reservation funding the new
	// asset-leaf carriers. Inputs must contain exactly one matching
	// operator-funded input.
	Lease *OORCarrierLease
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

	// New asset leaves are always created exactly at the operator floor;
	// anything else the operator rejects at submit.
	if r.Recipients[0].Value != r.OutputFloor {
		return fmt.Errorf("taproot asset OOR receiver carrier must " +
			"equal the operator floor")
	}

	floatInputIndex, err := r.OperatorFundedInputIndex()
	if err != nil {
		return err
	}
	if floatInputIndex == assetInputIndex {
		return fmt.Errorf("taproot asset OOR float input cannot " +
			"carry the asset")
	}
	if err := r.validateLeaseBinding(floatInputIndex); err != nil {
		return err
	}

	if _, err := r.CarrierAllocation(); err != nil {
		return err
	}
	if r.BuildChangeRecipient == nil {
		return fmt.Errorf("taproot asset OOR change recipient " +
			"builder is required")
	}

	return nil
}

// validateLeaseBinding proves the operator-funded input spends exactly the
// leased float, so the planned outputs and the signed graph cannot diverge
// from the reservation the operator granted.
func (r *TaprootAssetOORPrepareRequest) validateLeaseBinding(
	floatInputIndex int) error {

	if err := r.Lease.Validate(); err != nil {
		return err
	}

	floatInput := &r.Inputs[floatInputIndex]
	switch {
	case floatInput.VTXO.Outpoint != r.Lease.Outpoint:
		return fmt.Errorf("taproot asset OOR float input outpoint " +
			"does not match the lease")

	case floatInput.VTXO.Amount != r.Lease.Value:
		return fmt.Errorf("taproot asset OOR float input value does " +
			"not match the lease")

	case !bytes.Equal(floatInput.VTXO.PkScript, r.Lease.PkScript):
		return fmt.Errorf("taproot asset OOR float input pkScript " +
			"does not match the lease")

	case !bytes.Equal(
		floatInput.VTXOPolicyTemplate, r.Lease.PolicyTemplate,
	):
		return fmt.Errorf("taproot asset OOR float input policy does " +
			"not match the lease")
	}

	return nil
}

// TaprootAssetCarrierPlan is the Bitcoin-side output plan of an
// operator-funded asset send. Every new asset leaf is created at the operator
// floor out of the leased float, and a spent leaf's carrier follows its
// origin: round-created carriers return to the sender as plain Bitcoin
// change, while OOR-created carriers were operator money and are reclaimed
// into the operator's change.
type TaprootAssetCarrierPlan struct {
	// AssetChange is the asset-change leaf carrier: the operator floor on
	// a partial send, zero on a full send.
	AssetChange btcutil.Amount

	// SenderChange is the sender's plain Bitcoin change: the summed
	// carriers of the round-created asset inputs. Zero means every spent
	// leaf was OOR-created and no sender change output exists.
	SenderChange btcutil.Amount

	// OperatorChange is the value returned to the lease pkScript: the
	// float residual (lease value minus the new asset-leaf floors) plus
	// the reclaimed carriers of the OOR-created asset inputs. Zero means
	// the lease was consumed exactly with nothing reclaimed and no
	// operator output exists.
	OperatorChange btcutil.Amount
}

// CarrierAllocation returns the Bitcoin-side output plan funded by the
// operator's carrier float. The recipient leaf and the asset-change leaf of a
// partial send are always the operator floor; a lease below the summed floors
// cannot fund the send. The carrier arithmetic sums over every asset input so
// atomic multi-input sends can reuse it unchanged.
func (r *TaprootAssetOORPrepareRequest) CarrierAllocation() (
	TaprootAssetCarrierPlan, error) {

	if r == nil || len(r.Recipients) != 1 {
		return TaprootAssetCarrierPlan{}, fmt.Errorf("taproot asset " +
			"OOR requires exactly one recipient")
	}
	if err := r.Lease.Validate(); err != nil {
		return TaprootAssetCarrierPlan{}, err
	}
	if r.OutputFloor <= 0 {
		return TaprootAssetCarrierPlan{}, fmt.Errorf("taproot asset " +
			"OOR output floor is required")
	}

	// A light scan suffices here: every caller runs the request's full
	// Validate (which runs AssetInputIndex) before planning values.
	if _, err := locateTaprootAssetInput(r.Inputs); err != nil {
		return TaprootAssetCarrierPlan{}, err
	}

	leafCount := r.Intent.NewAssetLeafCount()
	if leafCount <= 0 {
		return TaprootAssetCarrierPlan{}, fmt.Errorf("taproot asset " +
			"OOR requires at least one new asset leaf")
	}
	if r.OutputFloor > btcutil.MaxSatoshi/btcutil.Amount(leafCount) {
		return TaprootAssetCarrierPlan{}, fmt.Errorf("taproot asset " +
			"OOR carrier sum overflows")
	}

	floors := r.OutputFloor * btcutil.Amount(leafCount)
	if r.Lease.Value < floors {
		return TaprootAssetCarrierPlan{}, fmt.Errorf("taproot asset "+
			"OOR float lease value %d sat is below the %d sat of "+
			"new asset-leaf floors", r.Lease.Value, floors)
	}

	senderCarriers, reclaimedCarriers := splitAssetInputCarriers(r.Inputs)
	plan := TaprootAssetCarrierPlan{
		SenderChange:   senderCarriers,
		OperatorChange: r.Lease.Value - floors + reclaimedCarriers,
	}
	if leafCount > 1 {
		plan.AssetChange = r.OutputFloor
	}

	return plan, nil
}

// splitAssetInputCarriers sums the asset inputs' Bitcoin carriers by leaf
// origin: a round-created leaf's carrier is the sender's own money, while an
// OOR-created leaf's carrier was operator-funded and is reclaimed when the
// leaf is spent.
func splitAssetInputCarriers(inputs []TransferInput) (btcutil.Amount,
	btcutil.Amount) {

	var sender, reclaimed btcutil.Amount
	for idx := range inputs {
		input := &inputs[idx]
		if input.VTXO == nil || !hasTaprootAssetState(input) {
			continue
		}
		if input.TaprootAssetRoundCreated {
			sender += input.VTXO.Amount

			continue
		}
		reclaimed += input.VTXO.Amount
	}

	return sender, reclaimed
}

// hasTaprootAssetState reports whether any identity field marks the input as
// asset-bearing. The transfer-input and descriptor views must agree, which
// AssetInputIndex enforces separately.
func hasTaprootAssetState(input *TransferInput) bool {
	return input.TaprootAssetRoot != nil ||
		input.VTXO.TaprootAssetRoot != nil ||
		input.VTXO.TaprootAssetRef != "" ||
		input.VTXO.TaprootAssetAmount != 0
}

// locateTaprootAssetInput returns the unique asset-bearing input without the
// per-input structural validation AssetInputIndex performs.
func locateTaprootAssetInput(inputs []TransferInput) (int, error) {
	assetIndex := -1
	for idx := range inputs {
		input := &inputs[idx]
		if input.VTXO == nil {
			return 0, fmt.Errorf("taproot asset OOR input %d "+
				"has no VTXO", idx)
		}

		if !hasTaprootAssetState(input) {
			continue
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

// OperatorFundedInputIndex returns the unique operator-funded float input.
func (r *TaprootAssetOORPrepareRequest) OperatorFundedInputIndex() (int,
	error) {

	if r == nil {
		return 0, fmt.Errorf("taproot asset OOR prepare request is " +
			"required")
	}

	floatIndex := -1
	for idx := range r.Inputs {
		if !r.Inputs[idx].OperatorFunded {
			continue
		}
		if floatIndex >= 0 {
			return 0, fmt.Errorf("taproot asset OOR requires " +
				"exactly one operator-funded input")
		}
		floatIndex = idx
	}
	if floatIndex < 0 {
		return 0, fmt.Errorf("taproot asset OOR requires an " +
			"operator-funded float input")
	}

	return floatIndex, nil
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

		if !hasTaprootAssetState(input) {
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

	plan, err := request.CarrierAllocation()
	if err != nil {
		return err
	}

	var (
		assetTotal         uint64
		matchingCount      int
		operatorChangeSeen int
		senderChangeSeen   int
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

			// A non-asset output is either the operator's change
			// (pays the lease pkScript verbatim) or the sender's
			// returned carrier.
			if bytes.Equal(
				recipient.PkScript, request.Lease.PkScript,
			) {

				if recipient.Value != plan.OperatorChange {
					return fmt.Errorf("taproot asset OOR " +
						"operator change value " +
						"mismatch")
				}
				operatorChangeSeen++

				continue
			}

			// The sender's carrier returns only when the spent
			// leaf was round-created; a reclaim-only send must
			// not pay the sender any plain output.
			if plan.SenderChange == 0 {
				return fmt.Errorf("taproot asset OOR sender " +
					"change must be absent when every " +
					"carrier is reclaimed")
			}
			if recipient.Value != plan.SenderChange {
				return fmt.Errorf("taproot asset OOR sender " +
					"change value mismatch")
			}
			senderChangeSeen++

			continue
		}

		// Every new asset leaf is operator-funded at exactly the
		// floor, mirroring the operator's submit validation.
		if recipient.Value != request.OutputFloor {
			return fmt.Errorf("taproot asset OOR asset carrier " +
				"is not the operator floor")
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
	wantSenderChange := 0
	if plan.SenderChange > 0 {
		wantSenderChange = 1
	}
	if senderChangeSeen != wantSenderChange {
		return fmt.Errorf("taproot asset OOR sender change is not " +
			"unique")
	}

	wantOperatorChange := 0
	if plan.OperatorChange > 0 {
		wantOperatorChange = 1
	}
	if operatorChangeSeen != wantOperatorChange {
		return fmt.Errorf("taproot asset OOR operator change is not " +
			"unique")
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

// TaprootAssetOORResume contains the exact wallet input outpoints and the
// carrier lease journaled before the first external asset commit. The float
// input is not a wallet VTXO, so it is carried as the lease rather than an
// outpoint the wallet could re-select.
type TaprootAssetOORResume struct {
	InputOutpoints []wire.OutPoint
	Lease          *OORCarrierLease
}

// TaprootAssetOORPreparationResumer is an optional restart bridge implemented
// by durable preparers. A nil result means no side-effectful preparation needs
// adoption and ordinary wallet selection should proceed.
type TaprootAssetOORPreparationResumer interface {
	ResumeTaprootAssetOOR(context.Context,
		*TaprootAssetOORResumeRequest) (*TaprootAssetOORResume, error)
}
