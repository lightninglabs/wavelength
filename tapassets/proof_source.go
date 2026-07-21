package tapassets

import (
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
)

// ProofSourceKind identifies the durable base of a reconstructed asset proof
// path without exposing tap-sdk types outside this adapter package.
type ProofSourceKind uint8

const (
	// ProofSourceConfirmedFile starts at a complete confirmed proof file.
	ProofSourceConfirmedFile ProofSourceKind = 1

	// ProofSourceCompactPath extends an already checksummed compact path.
	ProofSourceCompactPath ProofSourceKind = 2
)

// AssetPacketRole identifies the virtual packet collection selected by a
// durable logical mapping.
type AssetPacketRole uint8

const (
	// AssetPacketActive identifies the active transfer packet collection.
	AssetPacketActive AssetPacketRole = 1

	// AssetPacketPassive identifies the passive re-anchor collection.
	AssetPacketPassive AssetPacketRole = 2
)

// CreatedAssetProofSource is the SDK-neutral, restart-stable material needed
// to spend one exact asset-bearing VTXO in a later custom-anchor transaction.
// Amount is measured in asset units; the carrier-satoshi amount remains on
// the VTXO descriptor.
type CreatedAssetProofSource struct {
	LogicalInputID     string
	LogicalInputIndex  uint32
	LogicalOutputID    string
	LogicalOutputIndex uint32
	PacketRole         AssetPacketRole
	PacketIndex        uint32
	VirtualOutputIndex uint32
	AnchorOutputIndex  uint32
	AnchorOutpoint     wire.OutPoint
	CarrierValueSat    int64
	AssetRef           string
	AssetAmount        uint64
	TaprootAssetRoot   chainhash.Hash
	ProofSourceKind    ProofSourceKind
	ProofSourceID      [32]byte
	ProofSourceBlob    []byte
	TransitionProof    []byte
	CompactProofPath   []byte
	OPTrueWitness      wire.TxWitness
}

// ResolveCreatedAssetProofSource validates a sealed tap-sdk package and
// resolves the exact proof path and OP_TRUE witness for a VTXO it created.
// The returned byte slices never alias packageBytes or tap-sdk-owned memory.
func ResolveCreatedAssetProofSource(packageBytes []byte, outpoint wire.OutPoint,
	carrierValueSat int64, assetRef string, assetAmount uint64,
	taprootAssetRoot chainhash.Hash) (*CreatedAssetProofSource, error) {

	if len(packageBytes) == 0 {
		return nil, fmt.Errorf("sealed Taproot Asset package is " +
			"required")
	}
	expectedRef, err := tapsdk.ParseAssetRef(assetRef)
	if err != nil {
		return nil, fmt.Errorf("parse Taproot Asset ref: %w", err)
	}
	if assetAmount == 0 {
		return nil, fmt.Errorf("Taproot Asset amount is required")
	}
	if carrierValueSat <= 0 {
		return nil, fmt.Errorf("carrier-satoshi value is required")
	}

	driver := &sdkDriver{}
	committed, err := driver.DecodePackage(packageBytes)
	if err != nil {
		return nil, err
	}

	return resolveCreatedAssetProofSource(
		committed, sdkOutpoint(outpoint), carrierValueSat, expectedRef,
		assetAmount, tapsdk.Hash(taprootAssetRoot),
	)
}

func resolveCreatedAssetProofSource(committed *commitResult,
	outpoint tapsdk.Outpoint, carrierValueSat int64,
	assetRef tapsdk.AssetRef, assetAmount uint64,
	taprootAssetRoot tapsdk.Hash) (*CreatedAssetProofSource, error) {

	if committed == nil {
		return nil, fmt.Errorf("committed Taproot Asset package is " +
			"required")
	}

	var selected *commitOutput
	for idx := range committed.outputs {
		output := &committed.outputs[idx]
		if output.anchorOutpoint != outpoint ||
			output.anchorValueSat != carrierValueSat ||
			!output.assetRef.Equivalent(assetRef) ||
			output.amount != assetAmount ||
			output.taprootAssetRoot != taprootAssetRoot {

			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("created Taproot Asset output " +
				"is ambiguous")
		}
		selected = output
	}
	if selected == nil {
		return nil, fmt.Errorf("sealed package does not create the " +
			"requested Taproot Asset output")
	}
	if selected.scriptMode != tapsdk.CustomAssetScriptOPTrue ||
		len(selected.opTrueWitness) == 0 {
		return nil, fmt.Errorf("created Taproot Asset output is not " +
			"spendable through OP_TRUE")
	}
	if len(selected.proofBlob) == 0 {
		return nil, fmt.Errorf("created Taproot Asset output has no " +
			"transition proof")
	}

	step := tapsdk.AssetProofPathStep{
		TransitionProof: append([]byte(nil), selected.proofBlob...),
	}
	stepSummary, err := step.Summary()
	if err != nil {
		return nil, fmt.Errorf("summarize created asset proof: %w", err)
	}
	if stepSummary.AnchorOutpoint != selected.anchorOutpoint ||
		!stepSummary.AssetRef.Equivalent(selected.assetRef) ||
		stepSummary.IssuanceID != selected.issuanceID ||
		stepSummary.Amount != selected.amount ||
		stepSummary.ScriptKey != selected.scriptKey ||
		stepSummary.AnchorValueSat != selected.anchorValueSat {
		return nil, fmt.Errorf("created asset proof does not match " +
			"package output")
	}

	var input *commitInput
	for idx := range committed.inputs {
		candidate := &committed.inputs[idx]
		if candidate.anchorOutpoint !=
			stepSummary.PreviousAnchorOutpoint ||
			!candidate.assetRef.Equivalent(stepSummary.AssetRef) ||
			candidate.issuanceID != stepSummary.IssuanceID {

			continue
		}
		if input != nil {
			return nil, fmt.Errorf("created asset proof has " +
				"multiple possible predecessor inputs")
		}
		input = candidate
	}
	if input == nil {
		return nil, fmt.Errorf("created asset proof predecessor is " +
			"not present in the sealed package")
	}

	path := &tapsdk.AssetProofPath{}
	sourceKind, err := proofPathFromSource(input.proofSource, path)
	if err != nil {
		return nil, err
	}
	path.Steps = append(path.Steps, step)
	compactPath, err := path.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("encode extended asset proof path: %w",
			err)
	}

	return &CreatedAssetProofSource{
		LogicalInputID:     input.logicalInputID,
		LogicalInputIndex:  input.logicalInputIndex,
		LogicalOutputID:    selected.logicalOutputID,
		LogicalOutputIndex: selected.logicalOutputIndex,
		PacketRole:         assetPacketRole(selected.packetRole),
		PacketIndex:        selected.packetIndex,
		VirtualOutputIndex: selected.virtualOutputIndex,
		AnchorOutputIndex:  selected.anchorOutputIndex,
		AnchorOutpoint: wire.OutPoint{
			Hash:  selected.anchorOutpoint.Txid,
			Index: selected.anchorOutpoint.Index,
		},
		CarrierValueSat:  selected.anchorValueSat,
		AssetRef:         selected.assetRef.String(),
		AssetAmount:      selected.amount,
		TaprootAssetRoot: chainhash.Hash(selected.taprootAssetRoot),
		ProofSourceKind:  sourceKind,
		ProofSourceID:    [32]byte(input.proofSource.contentID),
		ProofSourceBlob: append(
			[]byte(nil), input.proofSource.blob...,
		),
		TransitionProof:  append([]byte(nil), selected.proofBlob...),
		CompactProofPath: append([]byte(nil), compactPath...),
		OPTrueWitness: wire.TxWitness(
			cloneByteSlices(selected.opTrueWitness),
		),
	}, nil
}

func proofPathFromSource(source commitProofSource,
	path *tapsdk.AssetProofPath) (ProofSourceKind, error) {

	switch source.kind {
	case tapsdk.CustomAnchorProofSourceConfirmedFile:
		*path = tapsdk.AssetProofPath{
			Version: tapsdk.AssetProofPathVersionV0,
			ConfirmedBaseProof: append(
				[]byte(nil), source.blob...,
			),
		}

		return ProofSourceConfirmedFile, nil

	case tapsdk.CustomAnchorProofSourceCompactPath:
		if err := path.UnmarshalBinary(source.blob); err != nil {
			return 0, fmt.Errorf("decode predecessor asset proof "+
				"path: %w", err)
		}

		return ProofSourceCompactPath, nil

	default:
		return 0, fmt.Errorf("unsupported Taproot Asset proof "+
			"source %d", source.kind)
	}
}

func assetPacketRole(role tapsdk.CustomAnchorPacketRole) AssetPacketRole {
	switch role {
	case tapsdk.CustomAnchorPacketRoleActive:
		return AssetPacketActive

	case tapsdk.CustomAnchorPacketRolePassive:
		return AssetPacketPassive

	default:
		return 0
	}
}
