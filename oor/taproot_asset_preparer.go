package oor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
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

	// ProofFile is the complete confirmed proof for the selected asset.
	ProofFile []byte

	// RecipientScriptKey is the compressed Taproot Asset output script key.
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
	if len(i.ProofFile) == 0 {
		return fmt.Errorf("taproot asset input proof is required")
	}
	if len(i.ProofFile) > oortx.MaxTaprootAssetPackageBytes {
		return fmt.Errorf("taproot asset input proof exceeds %d bytes",
			oortx.MaxTaprootAssetPackageBytes)
	}
	if _, err := btcec.ParsePubKey(i.RecipientScriptKey); err != nil {
		return fmt.Errorf("taproot asset recipient script key: %w", err)
	}
	if len(i.ProofCourierAddress) >
		MaxTaprootAssetCourierAddressBytes {
		return fmt.Errorf("taproot asset proof courier address "+
			"exceeds %d bytes", MaxTaprootAssetCourierAddressBytes)
	}
	if len(i.ProofDeliveryMetadata) >
		MaxTaprootAssetProofDeliveryBytes {
		return fmt.Errorf("taproot asset proof delivery metadata "+
			"exceeds %d bytes", MaxTaprootAssetProofDeliveryBytes)
	}

	return nil
}

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
	// composed into their Taproot output keys.
	Recipients []oortx.RecipientOutput

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
	if len(r.Inputs) != 1 {
		return fmt.Errorf("taproot asset OOR requires exactly one " +
			"input")
	}
	if len(r.Recipients) != 1 {
		return fmt.Errorf("taproot asset OOR requires exactly one " +
			"recipient")
	}
	if err := r.Intent.Validate(); err != nil {
		return err
	}
	if err := r.Inputs[0].Validate(); err != nil {
		return fmt.Errorf("taproot asset OOR input: %w", err)
	}
	if r.Inputs[0].TaprootAssetRoot == nil {
		return fmt.Errorf("taproot asset OOR input root is required")
	}
	if r.Inputs[0].VTXO.Outpoint != r.Intent.InputVTXOOutpoint {
		return fmt.Errorf("taproot asset OOR input outpoint does not " +
			"match the requested managed VTXO")
	}
	if r.Recipients[0].Value != r.Inputs[0].VTXO.Amount {
		return fmt.Errorf("taproot asset OOR requires exact BTC value")
	}
	if len(r.Recipients[0].VTXOPolicyTemplate) == 0 {
		return fmt.Errorf("taproot asset OOR recipient policy is " +
			"required")
	}

	return nil
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
	if len(p.Recipients) != len(request.Recipients) {
		return fmt.Errorf("taproot asset OOR recipient count changed")
	}
	for idx := range p.Recipients {
		before := request.Recipients[idx]
		after := p.Recipients[idx]
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
