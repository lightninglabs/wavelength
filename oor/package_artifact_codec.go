package oor

import (
	"bytes"
	"fmt"

	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	packageArtifactSessionIDRecordType   tlv.Type = 1
	packageArtifactArkPSBTRecordType     tlv.Type = 3
	packageArtifactCheckpointsRecordType tlv.Type = 5
	packageArtifactAssetTransferType     tlv.Type = 7
)

// encodePackageArtifacts serializes a package-artifact slice as a length
// prefixed list of TLV-encoded package blobs.
func encodePackageArtifacts(artifacts []PackageArtifact) ([]byte, error) {
	if len(artifacts) == 0 {
		return nil, nil
	}

	blobs := make([][]byte, 0, len(artifacts))
	for i := range artifacts {
		raw, err := encodePackageArtifact(artifacts[i])
		if err != nil {
			return nil, err
		}

		blobs = append(blobs, raw)
	}

	return encodeLengthPrefixedBlobList(blobs)
}

// decodePackageArtifactsWithLimits deserializes package artifacts while
// applying the supplied receive limits to nested blob-list fields.
func decodePackageArtifactsWithLimits(raw []byte,
	limits ReceiveLimits) ([]PackageArtifact, error) {

	if len(raw) == 0 {
		return nil, nil
	}

	blobs, err := decodeLengthPrefixedBlobListWithLimits(raw, limits)
	if err != nil {
		return nil, err
	}

	artifacts := make([]PackageArtifact, 0, len(blobs))
	for i := range blobs {
		artifact, err := decodePackageArtifactWithLimits(
			blobs[i], limits,
		)
		if err != nil {
			return nil, err
		}

		artifacts = append(artifacts, artifact)
	}

	return artifacts, nil
}

// encodePackageArtifact serializes one package artifact as a TLV stream.
func encodePackageArtifact(artifact PackageArtifact) ([]byte, error) {
	if artifact.SessionID == (SessionID{}) {
		return nil, fmt.Errorf("package artifact session id required")
	}

	if artifact.ArkPSBT == nil {
		return nil, fmt.Errorf("package artifact ark psbt required")
	}

	if len(artifact.FinalCheckpointPSBTs) == 0 {
		return nil, fmt.Errorf("package artifact checkpoints required")
	}

	sessionBytes := sessionIDBytes(artifact.SessionID)
	arkPSBT, err := psbtutil.Serialize(artifact.ArkPSBT)
	if err != nil {
		return nil, err
	}

	checkpoints, err := serializePSBTSlice(
		artifact.FinalCheckpointPSBTs,
	)
	if err != nil {
		return nil, err
	}

	checkpointRaw, err := encodeLengthPrefixedBlobList(checkpoints)
	if err != nil {
		return nil, err
	}

	var assetTransfer []byte
	if artifact.TaprootAssetTransfer != nil {
		assetTransfer, err = artifact.TaprootAssetTransfer.
			MarshalBinary()
		if err != nil {
			return nil, err
		}
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			packageArtifactSessionIDRecordType, &sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			packageArtifactArkPSBTRecordType, &arkPSBT,
		),
		tlv.MakePrimitiveRecord(
			packageArtifactCheckpointsRecordType, &checkpointRaw,
		),
		tlv.MakePrimitiveRecord(
			packageArtifactAssetTransferType, &assetTransfer,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// decodePackageArtifactWithLimits deserializes one package artifact while
// applying the supplied receive limits to its checkpoint list.
func decodePackageArtifactWithLimits(raw []byte,
	limits ReceiveLimits) (PackageArtifact, error) {

	var (
		sessionBytes  []byte
		arkPSBT       []byte
		checkpoints   []byte
		assetTransfer []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			packageArtifactSessionIDRecordType, &sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			packageArtifactArkPSBTRecordType, &arkPSBT,
		),
		tlv.MakePrimitiveRecord(
			packageArtifactCheckpointsRecordType, &checkpoints,
		),
		tlv.MakePrimitiveRecord(
			packageArtifactAssetTransferType, &assetTransfer,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return PackageArtifact{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return PackageArtifact{}, err
	}

	sessionID, err := parseSessionID(sessionBytes)
	if err != nil {
		return PackageArtifact{}, err
	}

	ark, err := psbtutil.Parse(arkPSBT)
	if err != nil {
		return PackageArtifact{}, err
	}

	checkpointRaw, err := decodeLengthPrefixedBlobListWithLimits(
		checkpoints, limits,
	)
	if err != nil {
		return PackageArtifact{}, err
	}

	parsedCheckpoints, err := parsePSBTSlice(checkpointRaw)
	if err != nil {
		return PackageArtifact{}, err
	}

	decodedAssetTransfer, err := decodeTaprootAssetTransfer(
		assetTransfer, len(parsedCheckpoints),
	)
	if err != nil {
		return PackageArtifact{}, err
	}

	return PackageArtifact{
		SessionID:            sessionID,
		ArkPSBT:              ark,
		FinalCheckpointPSBTs: parsedCheckpoints,
		TaprootAssetTransfer: decodedAssetTransfer,
	}, nil
}
