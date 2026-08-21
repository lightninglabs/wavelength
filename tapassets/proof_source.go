package tapassets

import (
	"fmt"
	"sort"

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
	ScriptKey          [33]byte
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

	// The emitted transition's predecessor is vPacket input 0: the spine.
	// Every other same-asset input of the package is a co-input whose
	// prior state the transition consumed alongside the spine tip.
	var (
		input    *commitInput
		coInputs []*commitInput
	)
	for idx := range committed.inputs {
		candidate := &committed.inputs[idx]
		if !candidate.assetRef.Equivalent(stepSummary.AssetRef) ||
			candidate.issuanceID != stepSummary.IssuanceID {
			return nil, fmt.Errorf("sealed package carries a " +
				"foreign asset input")
		}
		if candidate.anchorOutpoint !=
			stepSummary.PreviousAnchorOutpoint {

			coInputs = append(coInputs, candidate)

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
	if len(coInputs) > tapsdk.AssetProofPathMaxStepCoPaths {
		return nil, fmt.Errorf("created asset proof merges %d "+
			"co-inputs, more than %d", len(coInputs),
			tapsdk.AssetProofPathMaxStepCoPaths)
	}

	// Co-tips are declared in the package's own input order, matching the
	// order the Ark request presented them in.
	sort.SliceStable(coInputs, func(i, j int) bool {
		return coInputs[i].logicalInputIndex <
			coInputs[j].logicalInputIndex
	})
	for _, coInput := range coInputs {
		coPath := &tapsdk.AssetProofPath{}
		if _, err := proofPathFromSource(
			coInput.proofSource, coPath,
		); err != nil {
			return nil, fmt.Errorf("co-input %d: %w",
				coInput.logicalInputIndex, err)
		}
		step.CoInputPaths = append(step.CoInputPaths, coPath)
	}

	path := &tapsdk.AssetProofPath{}
	sourceKind, err := proofPathFromSource(input.proofSource, path)
	if err != nil {
		return nil, err
	}
	if len(step.CoInputPaths) != 0 {
		if err := promoteAssetProofPathV2(path); err != nil {
			return nil, err
		}
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
		ScriptKey:        selected.scriptKey,
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

// promoteAssetProofPathV2 re-expresses a path at version 2 so a merging step
// can carry co-input paths. V1 additional bases become first-step co-input
// paths of stepless confirmed files, the SDK's own internal model, because a
// V2 path must leave AdditionalBaseProofs empty.
func promoteAssetProofPathV2(path *tapsdk.AssetProofPath) error {
	if path.Version == tapsdk.AssetProofPathVersionV2 {
		return nil
	}
	if len(path.AdditionalBaseProofs) != 0 {
		if len(path.Steps) == 0 {
			return fmt.Errorf("spine path carries additional " +
				"bases without their merging step")
		}
		for _, base := range path.AdditionalBaseProofs {
			path.Steps[0].CoInputPaths = append(
				path.Steps[0].CoInputPaths,
				&tapsdk.AssetProofPath{
					Version: tapsdk.
						AssetProofPathVersionV0,
					ConfirmedBaseProof: base,
				},
			)
		}
		path.AdditionalBaseProofs = nil
	}
	path.Version = tapsdk.AssetProofPathVersionV2

	return nil
}

// AssetProofPathAnchor identifies one unconfirmed anchor transaction of a
// compact path's transfer DAG. OutputIndex selects the transition's
// asset-bearing output within that transaction.
type AssetProofPathAnchor struct {
	Txid        chainhash.Hash
	OutputIndex uint32
}

// CollectAssetProofPathAnchors returns every unconfirmed anchor across the
// path's transfer DAG: the spine steps plus, recursively, every step's
// co-input paths. Anchors are deduplicated by txid so a shared ancestor
// resolves once.
func CollectAssetProofPathAnchors(path *tapsdk.AssetProofPath) (
	[]AssetProofPathAnchor, error) {

	if path == nil {
		return nil, fmt.Errorf("asset proof path is required")
	}

	seen := make(map[chainhash.Hash]struct{})
	var anchors []AssetProofPathAnchor
	var walk func(*tapsdk.AssetProofPath) error
	walk = func(current *tapsdk.AssetProofPath) error {
		for i := range current.Steps {
			summary, err := current.Steps[i].Summary()
			if err != nil {
				return fmt.Errorf("summarize lineage step "+
					"%d: %w", i, err)
			}
			txid := chainhash.Hash(summary.AnchorOutpoint.Txid)
			if _, ok := seen[txid]; !ok {
				seen[txid] = struct{}{}
				anchors = append(anchors, AssetProofPathAnchor{
					Txid: txid,
					OutputIndex: summary.
						AnchorOutpoint.Index,
				})
			}
			for _, coPath := range current.Steps[i].CoInputPaths {
				if err := walk(coPath); err != nil {
					return err
				}
			}
		}

		return nil
	}
	if err := walk(path); err != nil {
		return nil, err
	}

	return anchors, nil
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
