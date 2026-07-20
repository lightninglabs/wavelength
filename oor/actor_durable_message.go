package oor

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	startPayloadOperatorKeyRecordType tlv.Type = 1
	startPayloadCSVDelayRecordType    tlv.Type = 2
	startPayloadInputsRecordType      tlv.Type = 3
	startPayloadRecipientsRecordType  tlv.Type = 4

	// startPayloadIdempotencyKeyType stores the optional caller-provided
	// OOR send idempotency key.
	startPayloadIdempotencyKeyType tlv.Type = 5
)

const (
	sessionPayloadSessionIDRecordType tlv.Type = 1
)

const (
	// resumePayloadSessionIDRecordType stores the session id of a resume
	// request.
	resumePayloadSessionIDRecordType tlv.Type = 1

	// resumePayloadFromRetryTimerRecordType stores whether the resume was
	// driven by a fired retry timer (versus a boot restore). Only a timer
	// expiry advances the give-up attempt counter; a boot resume merely
	// re-arms the timer from the persisted count.
	resumePayloadFromRetryTimerRecordType tlv.Type = 3
)

const (
	// listSessionsDirectionRecordType stores the session direction filter.
	listSessionsDirectionRecordType tlv.Type = 1

	// listSessionsPendingOnlyRecordType stores the pending-only filter.
	listSessionsPendingOnlyRecordType tlv.Type = 3
)

const (
	resolveIncomingPayloadSessionIDRecordType tlv.Type = 1
	resolveIncomingPayloadPkScriptRecordType  tlv.Type = 2
	resolveIncomingPayloadEventIDRecordType   tlv.Type = 3
)

const (
	restorePayloadSnapshotRecordType tlv.Type = 1
)

const (
	driveEventPayloadSessionIDRecordType tlv.Type = 1
	driveEventPayloadEventRecordType     tlv.Type = 3
)

const (
	eventPayloadKindRecordType             tlv.Type = 1
	eventPayloadSubmitSessionIDRecordType  tlv.Type = 3
	eventPayloadArkPSBTRecordType          tlv.Type = 5
	eventPayloadCheckpointPSBTsRecordType  tlv.Type = 7
	eventPayloadReasonRecordType           tlv.Type = 9
	eventPayloadOutpointsRecordType        tlv.Type = 11
	eventPayloadMetadataMatchesRecordType  tlv.Type = 13
	eventPayloadAncestorPackagesRecordType tlv.Type = 15
	eventPayloadOutboxTypeRecordType       tlv.Type = 17
	eventPayloadRetryableRecordType        tlv.Type = 19
	eventPayloadRetryAfterNanosRecordType  tlv.Type = 21
	eventPayloadIncomingRecipientsType     tlv.Type = 23
)

const (
	eventKindSubmitAccepted    uint64 = 1
	eventKindCheckpointsSigned uint64 = 2
	eventKindFinalizeAccepted  uint64 = 3
	eventKindInputsMarkedSpent uint64 = 4
	eventKindFail              uint64 = 5
	eventKindIncomingTransfer  uint64 = 7
	eventKindIncomingHandled   uint64 = 8
	eventKindIncomingAckSent   uint64 = 9
	eventKindIncomingMetadata  uint64 = 10
	eventKindOutboxError       uint64 = 11
	eventKindArkSigned         uint64 = 12
)

const (
	transferInputOutpointRecordType           tlv.Type = 1
	transferInputAmountSatRecordType          tlv.Type = 2
	transferInputClientFamilyRecordType       tlv.Type = 3
	transferInputClientIndexRecordType        tlv.Type = 4
	transferInputClientPubKeyRecordType       tlv.Type = 5
	transferInputOperatorPubKeyRecordType     tlv.Type = 6
	transferInputExitDelayRecordType          tlv.Type = 7
	transferInputOwnerLeafScriptRecordType    tlv.Type = 8
	transferInputPkScriptRecordType           tlv.Type = 9
	transferInputSpendWitnessScriptRecordType tlv.Type = 10
	transferInputSpendControlBlockRecordType  tlv.Type = 11
	transferInputConditionWitnessRecordType   tlv.Type = 12
	transferInputOwnerLeafPolicyRecordType    tlv.Type = 13
	transferInputVTXOPolicyRecordType         tlv.Type = 14
	transferInputRequiredSequenceRecordType   tlv.Type = 15
	transferInputRequiredLockTimeRecordType   tlv.Type = 16
	transferInputExternalSignaturesRecordType tlv.Type = 17
)

const (
	recipientPkScriptRecordType    tlv.Type = 1
	recipientValueSatRecordType    tlv.Type = 2
	recipientVTXOPolicyRecordType  tlv.Type = 3
	recipientOutputIndexRecordType tlv.Type = 5
)

const (
	incomingMetadataMatchOutputIndexRecordType    tlv.Type = 1
	incomingMetadataMatchRoundIDRecordType        tlv.Type = 3
	incomingMetadataMatchCommitmentTxIDRecordType tlv.Type = 5
	incomingMetadataMatchBatchExpiryRecordType    tlv.Type = 7
	incomingMetadataMatchChainDepthRecordType     tlv.Type = 11
	incomingMetadataMatchCreatedHeightRecordType  tlv.Type = 13
	incomingMetadataMatchAncestryPathsRecordType  tlv.Type = 17
	incomingMetadataMatchOperatorKeyRecordType    tlv.Type = 19
	incomingMetadataMatchBatchEvidenceRecordType  tlv.Type = 23
)

const (
	batchEvidenceTxIDRecordType        tlv.Type = 1
	batchEvidenceTxRecordType          tlv.Type = 3
	batchEvidenceOutputIndexRecordType tlv.Type = 5
	batchEvidencePkScriptRecordType    tlv.Type = 7
	batchEvidenceCSVDeltaRecordType    tlv.Type = 9
	batchEvidenceInputsRecordType      tlv.Type = 11
	batchEvidenceWatchHeightRecordType tlv.Type = 13
)

const (
	batchEvidenceInputOutpointRecordType tlv.Type = 1
	batchEvidenceInputValueRecordType    tlv.Type = 3
	batchEvidenceInputPkScriptRecordType tlv.Type = 5
)

type startTransferPayload struct {
	OperatorPubKey []byte
	CSVDelay       uint32
	Inputs         []*TransferInputSnapshot
	Recipients     []recipientPayload
	IdempotencyKey string
}

type recipientPayload struct {
	PkScript           []byte
	ValueSat           int64
	VTXOPolicyTemplate []byte
}

type incomingRecipientPayload struct {
	OutputIndex        uint32
	PkScript           []byte
	ValueSat           int64
	VTXOPolicyTemplate []byte
}

func encodeStartTransferPayload(payload startTransferPayload) ([]byte, error) {
	inputs, err := encodeTransferInputSnapshots(payload.Inputs)
	if err != nil {
		return nil, err
	}

	recipients, err := encodeRecipientPayloads(payload.Recipients)
	if err != nil {
		return nil, err
	}

	operatorKey := payload.OperatorPubKey
	csvDelay := payload.CSVDelay
	idempotencyKey := []byte(payload.IdempotencyKey)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			startPayloadOperatorKeyRecordType, &operatorKey,
		),
		tlv.MakePrimitiveRecord(
			startPayloadCSVDelayRecordType, &csvDelay,
		),
		tlv.MakePrimitiveRecord(
			startPayloadInputsRecordType, &inputs,
		),
		tlv.MakePrimitiveRecord(
			startPayloadRecipientsRecordType, &recipients,
		),
		tlv.MakePrimitiveRecord(
			startPayloadIdempotencyKeyType, &idempotencyKey,
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

func decodeStartTransferPayload(raw []byte) (startTransferPayload, error) {
	return decodeStartTransferPayloadWithLimits(
		raw, ReceiveLimits{},
	)
}

// decodeStartTransferPayloadWithLimits decodes a start-transfer payload and
// applies receive limits to nested input and recipient lists.
func decodeStartTransferPayloadWithLimits(raw []byte,
	limits ReceiveLimits) (startTransferPayload, error) {

	var (
		operatorKey []byte
		csvDelay    uint32
		inputsRaw   []byte
		recipients  []byte
		idKey       []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			startPayloadOperatorKeyRecordType, &operatorKey,
		),
		tlv.MakePrimitiveRecord(
			startPayloadCSVDelayRecordType, &csvDelay,
		),
		tlv.MakePrimitiveRecord(
			startPayloadInputsRecordType, &inputsRaw,
		),
		tlv.MakePrimitiveRecord(
			startPayloadRecipientsRecordType, &recipients,
		),
		tlv.MakePrimitiveRecord(startPayloadIdempotencyKeyType, &idKey),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return startTransferPayload{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return startTransferPayload{}, err
	}

	inputs, err := decodeTransferInputSnapshotsWithLimits(
		inputsRaw, limits,
	)
	if err != nil {
		return startTransferPayload{}, err
	}

	recipientsPayload, err := decodeRecipientPayloadsWithLimits(
		recipients, limits,
	)
	if err != nil {
		return startTransferPayload{}, err
	}

	return startTransferPayload{
		OperatorPubKey: operatorKey,
		CSVDelay:       csvDelay,
		Inputs:         inputs,
		Recipients:     recipientsPayload,
		IdempotencyKey: string(idKey),
	}, nil
}

func encodeRecipientPayloads(payloads []recipientPayload) ([]byte, error) {
	blobs := make([][]byte, 0, len(payloads))
	for i := range payloads {
		raw, err := encodeRecipientPayload(payloads[i])
		if err != nil {
			return nil, err
		}

		blobs = append(blobs, raw)
	}

	return encodeLengthPrefixedBlobList(blobs)
}

// decodeRecipientPayloadsWithLimits decodes recipient payloads using the
// supplied receive limits for the outer blob list.
func decodeRecipientPayloadsWithLimits(raw []byte,
	limits ReceiveLimits) ([]recipientPayload, error) {

	blobs, err := decodeLengthPrefixedBlobListWithLimits(raw, limits)
	if err != nil {
		return nil, err
	}

	payloads := make([]recipientPayload, 0, len(blobs))
	for i := range blobs {
		payload, err := decodeRecipientPayload(blobs[i])
		if err != nil {
			return nil, err
		}

		payloads = append(payloads, payload)
	}

	return payloads, nil
}

func encodeOutPointList(outpoints []wire.OutPoint) ([]byte, error) {
	blobs := make([][]byte, 0, len(outpoints))

	for _, outpoint := range outpoints {
		blobs = append(blobs, outPointBytes(outpoint))
	}

	return encodeLengthPrefixedBlobList(blobs)
}

// decodeOutPointListWithLimits decodes outpoints using the supplied receive
// limits for the outer blob list.
func decodeOutPointListWithLimits(raw []byte,
	limits ReceiveLimits) ([]wire.OutPoint, error) {

	blobs, err := decodeLengthPrefixedBlobListWithLimits(raw, limits)
	if err != nil {
		return nil, err
	}

	outpoints := make([]wire.OutPoint, 0, len(blobs))
	for _, blob := range blobs {
		outpoint, err := parseOutPointBytes(blob)
		if err != nil {
			return nil, err
		}

		outpoints = append(outpoints, outpoint)
	}

	return outpoints, nil
}

func encodeIncomingMetadataMatches(matches []IncomingMetadataMatch) ([]byte,
	error) {

	blobs := make([][]byte, 0, len(matches))
	for i := range matches {
		raw, err := encodeIncomingMetadataMatch(matches[i])
		if err != nil {
			return nil, err
		}

		blobs = append(blobs, raw)
	}

	return encodeLengthPrefixedBlobList(blobs)
}

// decodeIncomingMetadataMatchesWithLimits decodes incoming metadata matches
// using the supplied receive limits for nested metadata and ancestry lists.
func decodeIncomingMetadataMatchesWithLimits(raw []byte,
	limits ReceiveLimits) ([]IncomingMetadataMatch, error) {

	blobs, err := decodeLengthPrefixedBlobListWithLimits(raw, limits)
	if err != nil {
		return nil, err
	}

	matches := make([]IncomingMetadataMatch, 0, len(blobs))
	for i := range blobs {
		match, err := decodeIncomingMetadataMatchWithLimits(
			blobs[i], limits,
		)
		if err != nil {
			return nil, err
		}

		matches = append(matches, match)
	}

	return matches, nil
}

// Per-Ancestry TLV record types used inside the ancestry-paths blob list.
// Tags are scoped to a single ancestry record so they do not conflict with
// the outer IncomingMetadataMatch record-type space.
const (
	ancestryPathTreePathRecordType       tlv.Type = 1
	ancestryPathCommitmentTxIDRecordType tlv.Type = 3
	ancestryPathInputIndicesRecordType   tlv.Type = 5
	ancestryPathTreeDepthRecordType      tlv.Type = 7
)

// encodeAncestryList encodes []vtxo.Ancestry as a length-prefixed blob
// list. Each blob is a TLV stream encoding one Ancestry's fields. The
// outer length prefix lets the durable mailbox replay tolerate additive
// changes to the per-ancestry record set without breaking older blobs.
func encodeAncestryList(ancestry []vtxo.Ancestry) ([]byte, error) {
	blobs := make([][]byte, 0, len(ancestry))
	for i := range ancestry {
		raw, err := encodeAncestryEntry(ancestry[i])
		if err != nil {
			return nil, err
		}
		blobs = append(blobs, raw)
	}

	return encodeLengthPrefixedBlobList(blobs)
}

// decodeAncestryListWithLimits decodes ancestry entries using the supplied
// receive limits for the outer blob list.
func decodeAncestryListWithLimits(raw []byte,
	limits ReceiveLimits) ([]vtxo.Ancestry, error) {

	if len(raw) == 0 {
		return nil, nil
	}

	blobs, err := decodeLengthPrefixedBlobListWithLimits(raw, limits)
	if err != nil {
		return nil, err
	}

	ancestry := make([]vtxo.Ancestry, 0, len(blobs))
	for i := range blobs {
		entry, err := decodeAncestryEntry(blobs[i])
		if err != nil {
			return nil, err
		}
		ancestry = append(ancestry, entry)
	}

	return ancestry, nil
}

// encodeAncestryEntry encodes one vtxo.Ancestry into a TLV blob.
func encodeAncestryEntry(a vtxo.Ancestry) ([]byte, error) {
	var treePath []byte
	if a.TreePath != nil {
		var err error
		treePath, err = clientdb.SerializeTree(a.TreePath)
		if err != nil {
			return nil, err
		}
	}

	commitmentTxID := a.CommitmentTxID[:]

	// Serialize input_indices as a length-prefixed list of uint32.
	indices := encodeUint32List(a.InputIndices)
	treeDepth := a.TreeDepth

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			ancestryPathTreePathRecordType, &treePath,
		),
		tlv.MakePrimitiveRecord(
			ancestryPathCommitmentTxIDRecordType, &commitmentTxID,
		),
		tlv.MakePrimitiveRecord(
			ancestryPathInputIndicesRecordType, &indices,
		),
		tlv.MakePrimitiveRecord(
			ancestryPathTreeDepthRecordType, &treeDepth,
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

// decodeAncestryEntry is the inverse of encodeAncestryEntry.
func decodeAncestryEntry(raw []byte) (vtxo.Ancestry, error) {
	var (
		treePath       []byte
		commitmentTxID []byte
		indicesRaw     []byte
		treeDepth      uint32
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			ancestryPathTreePathRecordType, &treePath,
		),
		tlv.MakePrimitiveRecord(
			ancestryPathCommitmentTxIDRecordType, &commitmentTxID,
		),
		tlv.MakePrimitiveRecord(
			ancestryPathInputIndicesRecordType, &indicesRaw,
		),
		tlv.MakePrimitiveRecord(
			ancestryPathTreeDepthRecordType, &treeDepth,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return vtxo.Ancestry{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return vtxo.Ancestry{}, err
	}

	if len(commitmentTxID) != chainhash.HashSize {
		return vtxo.Ancestry{}, fmt.Errorf("ancestry path commitment "+
			"txid must be %d bytes, got %d", chainhash.HashSize,
			len(commitmentTxID))
	}

	var decodedCommitmentTxID chainhash.Hash
	copy(decodedCommitmentTxID[:], commitmentTxID)

	indices, err := decodeUint32List(indicesRaw)
	if err != nil {
		return vtxo.Ancestry{}, err
	}

	var decodedTreePath *tree.Tree
	if len(treePath) > 0 {
		decodedTreePath, err = clientdb.DeserializeTree(treePath)
		if err != nil {
			return vtxo.Ancestry{}, err
		}
	}

	return vtxo.Ancestry{
		TreePath:       decodedTreePath,
		CommitmentTxID: decodedCommitmentTxID,
		InputIndices:   indices,
		TreeDepth:      treeDepth,
	}, nil
}

// encodeUint32List packs a slice of uint32 as len-prefixed big-endian bytes.
// Empty/nil slices encode to a single zero byte (count=0).
func encodeUint32List(values []uint32) []byte {
	buf := make([]byte, 0, 4+len(values)*4)

	count := uint32(len(values))
	buf = append(
		buf, byte(count>>24), byte(count>>16), byte(count>>8),
		byte(count),
	)
	for _, v := range values {
		buf = append(
			buf, byte(v>>24), byte(v>>16), byte(v>>8), byte(v),
		)
	}

	return buf
}

// decodeUint32List inverts encodeUint32List, returning a fresh slice.
func decodeUint32List(raw []byte) ([]uint32, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	if len(raw) < 4 {
		return nil, fmt.Errorf("uint32 list missing length prefix "+
			"(got %d bytes)", len(raw))
	}

	count := uint32(raw[0])<<24 | uint32(raw[1])<<16 |
		uint32(raw[2])<<8 | uint32(raw[3])

	// Compute the implied size in uint64 so a malicious count cannot
	// wrap int(count)*4 on 32-bit platforms and slip past the bounds
	// check. The TLV blob is sourced from the durable mailbox so a
	// crafted or corrupt record could otherwise crash the actor on
	// the make() below.
	implied := 4 + uint64(count)*4
	if uint64(len(raw)) != implied {
		return nil, fmt.Errorf("uint32 list length mismatch: count %d "+
			"implies %d bytes, got %d", count, implied, len(raw))
	}

	out := make([]uint32, count)
	for i := range out {
		off := 4 + i*4
		out[i] = uint32(raw[off])<<24 | uint32(raw[off+1])<<16 |
			uint32(raw[off+2])<<8 | uint32(raw[off+3])
	}

	return out, nil
}

// encodeBatchEvidenceList encodes each immutable evidence item as its own TLV
// stream so future additive fields remain replay-compatible.
func encodeBatchEvidenceList(evidence []batchcanon.BatchEvidence) ([]byte,
	error) {

	blobs := make([][]byte, 0, len(evidence))
	for i := range evidence {
		raw, err := encodeBatchEvidence(evidence[i])
		if err != nil {
			return nil, fmt.Errorf("encode batch evidence %d: %w",
				i, err)
		}

		blobs = append(blobs, raw)
	}

	return encodeLengthPrefixedBlobList(blobs)
}

// decodeBatchEvidenceList decodes and validates durable evidence before it is
// returned to the receive FSM.
func decodeBatchEvidenceList(raw []byte,
	limits ReceiveLimits) ([]batchcanon.BatchEvidence, error) {

	if len(raw) == 0 {
		return nil, nil
	}

	blobs, err := decodeLengthPrefixedBlobListWithLimits(raw, limits)
	if err != nil {
		return nil, err
	}

	evidence := make([]batchcanon.BatchEvidence, 0, len(blobs))
	for i := range blobs {
		item, err := decodeBatchEvidence(blobs[i], limits)
		if err != nil {
			return nil, fmt.Errorf("decode batch evidence %d: %w",
				i, err)
		}

		evidence = append(evidence, item)
	}

	return evidence, nil
}

// encodeBatchEvidence serializes one batch's authenticated watch inputs.
func encodeBatchEvidence(evidence batchcanon.BatchEvidence) ([]byte, error) {
	if err := evidence.Validate(); err != nil {
		return nil, err
	}
	if evidence.CSVExpiryDelta <= 0 {
		return nil, fmt.Errorf("batch evidence CSV expiry delta must " +
			"be positive")
	}

	txid := evidence.BatchTxID[:]
	tx := evidence.BatchTx
	outputIndex := evidence.BatchOutputIndex
	pkScript := evidence.ConfirmationPkScript
	csvDelta := uint32(evidence.CSVExpiryDelta)
	watchHeight := evidence.WatchHeightHint
	inputs, err := encodeBatchEvidenceInputs(evidence.ConsumedInputs)
	if err != nil {
		return nil, err
	}

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(batchEvidenceTxIDRecordType, &txid),
		tlv.MakePrimitiveRecord(batchEvidenceTxRecordType, &tx),
		tlv.MakePrimitiveRecord(
			batchEvidenceOutputIndexRecordType, &outputIndex,
		),
		tlv.MakePrimitiveRecord(
			batchEvidencePkScriptRecordType, &pkScript,
		),
		tlv.MakePrimitiveRecord(
			batchEvidenceCSVDeltaRecordType, &csvDelta,
		),
		tlv.MakePrimitiveRecord(
			batchEvidenceInputsRecordType, &inputs,
		),
		tlv.MakePrimitiveRecord(
			batchEvidenceWatchHeightRecordType, &watchHeight,
		),
	)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// decodeBatchEvidence reconstructs one evidence item from its TLV stream.
func decodeBatchEvidence(raw []byte,
	limits ReceiveLimits) (batchcanon.BatchEvidence, error) {

	var (
		txid        []byte
		tx          []byte
		outputIndex uint32
		pkScript    []byte
		csvDelta    uint32
		inputs      []byte
		watchHeight uint32
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(batchEvidenceTxIDRecordType, &txid),
		tlv.MakePrimitiveRecord(batchEvidenceTxRecordType, &tx),
		tlv.MakePrimitiveRecord(
			batchEvidenceOutputIndexRecordType, &outputIndex,
		),
		tlv.MakePrimitiveRecord(
			batchEvidencePkScriptRecordType, &pkScript,
		),
		tlv.MakePrimitiveRecord(
			batchEvidenceCSVDeltaRecordType, &csvDelta,
		),
		tlv.MakePrimitiveRecord(
			batchEvidenceInputsRecordType, &inputs,
		),
		tlv.MakePrimitiveRecord(
			batchEvidenceWatchHeightRecordType, &watchHeight,
		),
	)
	if err != nil {
		return batchcanon.BatchEvidence{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return batchcanon.BatchEvidence{}, err
	}
	if len(txid) != chainhash.HashSize {
		return batchcanon.BatchEvidence{}, fmt.Errorf("batch " +
			"evidence txid must be provided")
	}

	decodedCSVDelta, err := uint32ToInt32(
		csvDelta, "batch evidence CSV expiry delta",
	)
	if err != nil {
		return batchcanon.BatchEvidence{}, err
	}

	consumedInputs, err := decodeBatchEvidenceInputs(inputs, limits)
	if err != nil {
		return batchcanon.BatchEvidence{}, err
	}

	var decodedTxID chainhash.Hash
	copy(decodedTxID[:], txid)
	evidence := batchcanon.BatchEvidence{
		BatchTxID:            decodedTxID,
		BatchTx:              bytes.Clone(tx),
		BatchOutputIndex:     outputIndex,
		ConfirmationPkScript: bytes.Clone(pkScript),
		WatchHeightHint:      watchHeight,
		CSVExpiryDelta:       decodedCSVDelta,
		ConsumedInputs:       consumedInputs,
	}
	if evidence.CSVExpiryDelta <= 0 {
		return batchcanon.BatchEvidence{}, fmt.Errorf("batch " +
			"evidence CSV expiry delta must be positive")
	}
	if err := evidence.Validate(); err != nil {
		return batchcanon.BatchEvidence{}, err
	}

	return evidence, nil
}

// encodeBatchEvidenceInputs serializes every previous output as a nested TLV
// record, preserving transaction-input order.
func encodeBatchEvidenceInputs(inputs []batchcanon.ConsumedInput) ([]byte,
	error) {

	blobs := make([][]byte, 0, len(inputs))
	for i := range inputs {
		input := inputs[i]
		if input.Value < 0 {
			return nil, fmt.Errorf("batch evidence input %d has "+
				"negative value", i)
		}

		outpoint := outPointBytes(input.Outpoint)
		value := uint64(input.Value)
		pkScript := input.PkScript
		stream, err := tlv.NewStream(
			tlv.MakePrimitiveRecord(
				batchEvidenceInputOutpointRecordType, &outpoint,
			),
			tlv.MakePrimitiveRecord(
				batchEvidenceInputValueRecordType, &value,
			),
			tlv.MakePrimitiveRecord(
				batchEvidenceInputPkScriptRecordType, &pkScript,
			),
		)
		if err != nil {
			return nil, err
		}

		var buf bytes.Buffer
		if err := stream.Encode(&buf); err != nil {
			return nil, err
		}
		blobs = append(blobs, buf.Bytes())
	}

	return encodeLengthPrefixedBlobList(blobs)
}

// decodeBatchEvidenceInputs decodes the ordered previous-output records.
func decodeBatchEvidenceInputs(raw []byte,
	limits ReceiveLimits) ([]batchcanon.ConsumedInput, error) {

	blobs, err := decodeLengthPrefixedBlobListWithLimits(raw, limits)
	if err != nil {
		return nil, err
	}

	inputs := make([]batchcanon.ConsumedInput, 0, len(blobs))
	for i := range blobs {
		var (
			outpoint []byte
			value    uint64
			pkScript []byte
		)
		stream, err := tlv.NewStream(
			tlv.MakePrimitiveRecord(
				batchEvidenceInputOutpointRecordType, &outpoint,
			),
			tlv.MakePrimitiveRecord(
				batchEvidenceInputValueRecordType, &value,
			),
			tlv.MakePrimitiveRecord(
				batchEvidenceInputPkScriptRecordType, &pkScript,
			),
		)
		if err != nil {
			return nil, err
		}

		reader := bytes.NewReader(blobs[i])
		if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
			return nil, err
		}
		decodedOutpoint, err := parseOutPointBytes(outpoint)
		if err != nil {
			return nil, err
		}
		if value > math.MaxInt64 {
			return nil, fmt.Errorf("batch evidence input %d value "+
				"overflows int64", i)
		}

		inputs = append(inputs, batchcanon.ConsumedInput{
			Outpoint: decodedOutpoint,
			Value:    int64(value),
			PkScript: bytes.Clone(pkScript),
		})
	}

	return inputs, nil
}

func encodeIncomingMetadataMatch(match IncomingMetadataMatch) ([]byte, error) {
	outputIndex := match.OutputIndex
	roundID := []byte(match.Metadata.RoundID)
	commitmentTxID := match.Metadata.CommitmentTxID[:]
	batchExpiry := uint32(match.Metadata.BatchExpiry)
	chainDepth := uint32(match.Metadata.ChainDepth)
	createdHeight := uint32(match.Metadata.CreatedHeight)
	operatorKey := encodeOptionalPubKey(match.Metadata.OperatorKey)

	ancestryBytes, err := encodeAncestryList(match.Metadata.Ancestry)
	if err != nil {
		return nil, err
	}
	batchEvidence, err := encodeBatchEvidenceList(
		match.Metadata.BatchEvidence,
	)
	if err != nil {
		return nil, err
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchOutputIndexRecordType,
			&outputIndex,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchRoundIDRecordType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchCommitmentTxIDRecordType,
			&commitmentTxID,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchBatchExpiryRecordType,
			&batchExpiry,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchChainDepthRecordType, &chainDepth,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchCreatedHeightRecordType,
			&createdHeight,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchAncestryPathsRecordType,
			&ancestryBytes,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchOperatorKeyRecordType,
			&operatorKey,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchBatchEvidenceRecordType,
			&batchEvidence,
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

// decodeIncomingMetadataMatchWithLimits decodes one incoming metadata match
// and applies receive limits to its ancestry list.
func decodeIncomingMetadataMatchWithLimits(raw []byte,
	limits ReceiveLimits) (IncomingMetadataMatch, error) {

	var (
		outputIndex    uint32
		roundID        []byte
		commitmentTxID []byte
		batchExpiry    uint32
		chainDepth     uint32
		createdHeight  uint32
		ancestryBytes  []byte
		operatorKey    []byte
		batchEvidence  []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchOutputIndexRecordType,
			&outputIndex,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchRoundIDRecordType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchCommitmentTxIDRecordType,
			&commitmentTxID,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchBatchExpiryRecordType,
			&batchExpiry,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchChainDepthRecordType, &chainDepth,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchCreatedHeightRecordType,
			&createdHeight,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchAncestryPathsRecordType,
			&ancestryBytes,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchOperatorKeyRecordType,
			&operatorKey,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchBatchEvidenceRecordType,
			&batchEvidence,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return IncomingMetadataMatch{}, err
	}

	if len(commitmentTxID) != chainhash.HashSize {
		return IncomingMetadataMatch{}, fmt.Errorf("incoming " +
			"metadata commitment txid must be provided")
	}

	decodedBatchExpiry, err := uint32ToInt32(
		batchExpiry, "incoming batch expiry",
	)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	decodedChainDepth, err := uint32ToInt32(
		chainDepth, "incoming chain depth",
	)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	decodedCreatedHeight, err := uint32ToInt32(
		createdHeight, "incoming created height",
	)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	ancestry, err := decodeAncestryListWithLimits(ancestryBytes, limits)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	decodedOperatorKey, err := decodeOptionalPubKey(
		operatorKey, "incoming operator key",
	)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	decodedEvidence, err := decodeBatchEvidenceList(
		batchEvidence, limits,
	)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	var decodedCommitmentTxID chainhash.Hash
	copy(decodedCommitmentTxID[:], commitmentTxID)

	return IncomingMetadataMatch{
		OutputIndex: outputIndex,
		Metadata: IncomingVTXOMetadata{
			RoundID:        string(roundID),
			CommitmentTxID: decodedCommitmentTxID,
			BatchExpiry:    decodedBatchExpiry,
			ChainDepth:     int(decodedChainDepth),
			CreatedHeight:  decodedCreatedHeight,
			OperatorKey:    decodedOperatorKey,
			Ancestry:       ancestry,
			BatchEvidence:  decodedEvidence,
		},
	}, nil
}

// encodeOptionalPubKey returns the compressed encoding of pubKey, or nil when
// no key is present.
func encodeOptionalPubKey(pubKey *btcec.PublicKey) []byte {
	if pubKey == nil {
		return nil
	}

	return pubKey.SerializeCompressed()
}

// decodeOptionalPubKey parses a compressed public key when raw is non-empty.
func decodeOptionalPubKey(raw []byte, name string) (*btcec.PublicKey, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	pubKey, err := btcec.ParsePubKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}

	return pubKey, nil
}

func encodeRecipientPayload(payload recipientPayload) ([]byte, error) {
	pkScript := payload.PkScript
	if payload.ValueSat < 0 {
		return nil, fmt.Errorf("recipient value must be non-negative")
	}
	valueSat := uint64(payload.ValueSat)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(recipientPkScriptRecordType, &pkScript),
		tlv.MakePrimitiveRecord(recipientValueSatRecordType, &valueSat),
		tlv.MakePrimitiveRecord(
			recipientVTXOPolicyRecordType,
			&payload.VTXOPolicyTemplate,
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

func decodeRecipientPayload(raw []byte) (recipientPayload, error) {
	var (
		pkScript           []byte
		valueSat           uint64
		vtxoPolicyTemplate []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(recipientPkScriptRecordType, &pkScript),
		tlv.MakePrimitiveRecord(recipientValueSatRecordType, &valueSat),
		tlv.MakePrimitiveRecord(
			recipientVTXOPolicyRecordType, &vtxoPolicyTemplate,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return recipientPayload{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return recipientPayload{}, err
	}

	decodedValueSat, err := uint64ToInt64(
		valueSat, "recipient value sat",
	)
	if err != nil {
		return recipientPayload{}, err
	}

	return recipientPayload{
		PkScript:           pkScript,
		ValueSat:           decodedValueSat,
		VTXOPolicyTemplate: vtxoPolicyTemplate,
	}, nil
}

func encodeIncomingRecipients(recipients []ArkRecipientOutput) ([]byte, error) {
	payloads := make([][]byte, 0, len(recipients))
	for i := range recipients {
		raw, err := encodeIncomingRecipientPayload(
			incomingRecipientPayload{
				OutputIndex: recipients[i].OutputIndex,
				PkScript: append(
					[]byte(nil), recipients[i].PkScript...,
				),
				ValueSat: int64(recipients[i].Value),
				VTXOPolicyTemplate: append(
					[]byte(nil),
					recipients[i].VTXOPolicyTemplate...,
				),
			},
		)
		if err != nil {
			return nil, err
		}

		payloads = append(payloads, raw)
	}

	return encodeLengthPrefixedBlobList(payloads)
}

func decodeIncomingRecipientsWithLimits(raw []byte,
	limits ReceiveLimits) ([]ArkRecipientOutput, error) {

	if len(raw) == 0 {
		return nil, nil
	}

	blobs, err := decodeLengthPrefixedBlobListWithLimits(raw, limits)
	if err != nil {
		return nil, err
	}

	recipients := make([]ArkRecipientOutput, 0, len(blobs))
	for i := range blobs {
		payload, err := decodeIncomingRecipientPayload(blobs[i])
		if err != nil {
			return nil, err
		}

		recipients = append(recipients, ArkRecipientOutput{
			OutputIndex: payload.OutputIndex,
			Value:       btcutil.Amount(payload.ValueSat),
			PkScript: append(
				[]byte(nil), payload.PkScript...,
			),
			VTXOPolicyTemplate: append(
				[]byte(nil), payload.VTXOPolicyTemplate...,
			),
		})
	}

	return recipients, nil
}

func encodeIncomingRecipientPayload(payload incomingRecipientPayload) ([]byte,
	error) {

	pkScript := payload.PkScript
	if payload.ValueSat < 0 {
		return nil, fmt.Errorf("incoming recipient value must be " +
			"non-negative")
	}
	valueSat := uint64(payload.ValueSat)
	outputIndex := uint64(payload.OutputIndex)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			recipientPkScriptRecordType, &pkScript,
		),
		tlv.MakePrimitiveRecord(
			recipientValueSatRecordType, &valueSat,
		),
		tlv.MakePrimitiveRecord(
			recipientVTXOPolicyRecordType,
			&payload.VTXOPolicyTemplate,
		),
		tlv.MakePrimitiveRecord(
			recipientOutputIndexRecordType, &outputIndex,
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

func decodeIncomingRecipientPayload(raw []byte) (incomingRecipientPayload,
	error) {

	var (
		pkScript           []byte
		valueSat           uint64
		vtxoPolicyTemplate []byte
		outputIndex        uint64
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(recipientPkScriptRecordType, &pkScript),
		tlv.MakePrimitiveRecord(recipientValueSatRecordType, &valueSat),
		tlv.MakePrimitiveRecord(
			recipientVTXOPolicyRecordType, &vtxoPolicyTemplate,
		),
		tlv.MakePrimitiveRecord(
			recipientOutputIndexRecordType, &outputIndex,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return incomingRecipientPayload{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return incomingRecipientPayload{}, err
	}

	decodedValueSat, err := uint64ToInt64(
		valueSat, "incoming recipient value sat",
	)
	if err != nil {
		return incomingRecipientPayload{}, err
	}

	decodedOutputIndex, err := decodeUint64ToUint32(
		outputIndex, "incoming recipient output index",
	)
	if err != nil {
		return incomingRecipientPayload{}, err
	}

	return incomingRecipientPayload{
		OutputIndex:        decodedOutputIndex,
		PkScript:           pkScript,
		ValueSat:           decodedValueSat,
		VTXOPolicyTemplate: vtxoPolicyTemplate,
	}, nil
}

func encodeTransferInputSnapshots(inputs []*TransferInputSnapshot) ([]byte,
	error) {

	blobs := make([][]byte, 0, len(inputs))
	for i := range inputs {
		raw, err := encodeTransferInputSnapshot(inputs[i])
		if err != nil {
			return nil, err
		}

		blobs = append(blobs, raw)
	}

	return encodeLengthPrefixedBlobList(blobs)
}

// decodeTransferInputSnapshotsWithLimits decodes transfer-input snapshots
// using the supplied receive limits for the outer blob list.
func decodeTransferInputSnapshotsWithLimits(raw []byte,
	limits ReceiveLimits) ([]*TransferInputSnapshot, error) {

	blobs, err := decodeLengthPrefixedBlobListWithLimits(raw, limits)
	if err != nil {
		return nil, err
	}

	inputs := make([]*TransferInputSnapshot, 0, len(blobs))
	for i := range blobs {
		input, err := decodeTransferInputSnapshot(blobs[i])
		if err != nil {
			return nil, err
		}

		inputs = append(inputs, input)
	}

	return inputs, nil
}

func encodeTransferInputSnapshot(input *TransferInputSnapshot) ([]byte, error) {
	if input == nil {
		return nil, fmt.Errorf("transfer input snapshot must be " +
			"provided")
	}

	outpoint := outPointBytes(input.Outpoint)
	amountSat := uint64(input.AmountSat)
	clientFamily := uint32(input.ClientKeyFamily)
	clientIndex := input.ClientKeyIndex
	clientPubKey := input.ClientPubKey
	operatorPubKey := input.OperatorPubKey
	exitDelay := input.ExitDelay
	ownerLeafScript := input.OwnerLeafScript
	ownerLeafPolicy := input.OwnerLeafPolicy
	vtxoPolicyTemplate := input.VTXOPolicyTemplate
	requiredSequence := input.RequiredSequence
	requiredLockTime := input.RequiredLockTime

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			transferInputOutpointRecordType, &outpoint,
		),
		tlv.MakePrimitiveRecord(
			transferInputAmountSatRecordType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			transferInputClientFamilyRecordType, &clientFamily,
		),
		tlv.MakePrimitiveRecord(
			transferInputClientIndexRecordType, &clientIndex,
		),
		tlv.MakePrimitiveRecord(
			transferInputClientPubKeyRecordType, &clientPubKey,
		),
		tlv.MakePrimitiveRecord(
			transferInputOperatorPubKeyRecordType, &operatorPubKey,
		),
		tlv.MakePrimitiveRecord(
			transferInputExitDelayRecordType, &exitDelay,
		),
		tlv.MakePrimitiveRecord(
			transferInputOwnerLeafScriptRecordType,
			&ownerLeafScript,
		),
	}

	// Optional custom spend path fields.
	pkScript := input.PkScript
	if len(pkScript) > 0 {
		records = append(
			records, tlv.MakePrimitiveRecord(
				transferInputPkScriptRecordType, &pkScript,
			),
		)
	}

	witnessScript := input.SpendWitnessScript
	if len(witnessScript) > 0 {
		records = append(
			records, tlv.MakePrimitiveRecord(
				transferInputSpendWitnessScriptRecordType,
				&witnessScript,
			),
		)
	}

	controlBlock := input.SpendControlBlock
	if len(controlBlock) > 0 {
		records = append(
			records, tlv.MakePrimitiveRecord(
				transferInputSpendControlBlockRecordType,
				&controlBlock,
			),
		)
	}

	if len(input.ConditionWitness) > 0 {
		condBlob, condErr := encodeConditionWitness(
			input.ConditionWitness,
		)
		if condErr != nil {
			return nil, condErr
		}

		records = append(
			records, tlv.MakePrimitiveRecord(
				transferInputConditionWitnessRecordType,
				&condBlob,
			),
		)
	}

	if len(ownerLeafPolicy) > 0 {
		records = append(
			records, tlv.MakePrimitiveRecord(
				transferInputOwnerLeafPolicyRecordType,
				&ownerLeafPolicy,
			),
		)
	}

	if len(vtxoPolicyTemplate) > 0 {
		records = append(
			records, tlv.MakePrimitiveRecord(
				transferInputVTXOPolicyRecordType,
				&vtxoPolicyTemplate,
			),
		)
	}

	if requiredSequence != 0 {
		records = append(
			records, tlv.MakePrimitiveRecord(
				transferInputRequiredSequenceRecordType,
				&requiredSequence,
			),
		)
	}

	if requiredLockTime != 0 {
		records = append(
			records, tlv.MakePrimitiveRecord(
				transferInputRequiredLockTimeRecordType,
				&requiredLockTime,
			),
		)
	}

	if len(input.ExternalSignatures) > 0 {
		sigBlob, sigErr := encodeExternalSignatures(
			input.ExternalSignatures,
		)
		if sigErr != nil {
			return nil, sigErr
		}

		records = append(
			records, tlv.MakePrimitiveRecord(
				transferInputExternalSignaturesRecordType,
				&sigBlob,
			),
		)
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

func decodeTransferInputSnapshot(raw []byte) (*TransferInputSnapshot, error) {
	var (
		outpointRaw        []byte
		amountSat          uint64
		clientFamily       uint32
		clientIndex        uint32
		clientPubKey       []byte
		operatorPubKey     []byte
		exitDelay          uint32
		ownerLeafScript    []byte
		ownerLeafPolicy    []byte
		vtxoPolicyTemplate []byte
		pkScript           []byte
		witnessScript      []byte
		controlBlock       []byte
		condBlob           []byte
		externalSigBlob    []byte
		requiredSequence   uint32
		requiredLockTime   uint32
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			transferInputOutpointRecordType, &outpointRaw,
		),
		tlv.MakePrimitiveRecord(
			transferInputAmountSatRecordType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			transferInputClientFamilyRecordType, &clientFamily,
		),
		tlv.MakePrimitiveRecord(
			transferInputClientIndexRecordType, &clientIndex,
		),
		tlv.MakePrimitiveRecord(
			transferInputClientPubKeyRecordType, &clientPubKey,
		),
		tlv.MakePrimitiveRecord(
			transferInputOperatorPubKeyRecordType, &operatorPubKey,
		),
		tlv.MakePrimitiveRecord(
			transferInputExitDelayRecordType, &exitDelay,
		),
		tlv.MakePrimitiveRecord(
			transferInputOwnerLeafScriptRecordType,
			&ownerLeafScript,
		),
		tlv.MakePrimitiveRecord(
			transferInputPkScriptRecordType, &pkScript,
		),
		tlv.MakePrimitiveRecord(
			transferInputSpendWitnessScriptRecordType,
			&witnessScript,
		),
		tlv.MakePrimitiveRecord(
			transferInputSpendControlBlockRecordType, &controlBlock,
		),
		tlv.MakePrimitiveRecord(
			transferInputConditionWitnessRecordType, &condBlob,
		),
		tlv.MakePrimitiveRecord(
			transferInputOwnerLeafPolicyRecordType,
			&ownerLeafPolicy,
		),
		tlv.MakePrimitiveRecord(
			transferInputVTXOPolicyRecordType, &vtxoPolicyTemplate,
		),
		tlv.MakePrimitiveRecord(
			transferInputRequiredSequenceRecordType,
			&requiredSequence,
		),
		tlv.MakePrimitiveRecord(
			transferInputRequiredLockTimeRecordType,
			&requiredLockTime,
		),
		tlv.MakePrimitiveRecord(
			transferInputExternalSignaturesRecordType,
			&externalSigBlob,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return nil, err
	}

	outpoint, err := parseOutPointBytes(outpointRaw)
	if err != nil {
		return nil, err
	}

	decodedAmountSat, err := uint64ToInt64(amountSat, "amount sat")
	if err != nil {
		return nil, err
	}

	decodedClientFamily, err := uint32ToInt32(
		clientFamily, "client key family",
	)
	if err != nil {
		return nil, err
	}

	snap := &TransferInputSnapshot{
		Outpoint:           outpoint,
		AmountSat:          decodedAmountSat,
		ClientKeyFamily:    decodedClientFamily,
		ClientKeyIndex:     clientIndex,
		ClientPubKey:       clientPubKey,
		OperatorPubKey:     operatorPubKey,
		ExitDelay:          exitDelay,
		OwnerLeafScript:    ownerLeafScript,
		OwnerLeafPolicy:    ownerLeafPolicy,
		VTXOPolicyTemplate: vtxoPolicyTemplate,
		PkScript:           pkScript,
		SpendWitnessScript: witnessScript,
		SpendControlBlock:  controlBlock,
		RequiredSequence:   requiredSequence,
		RequiredLockTime:   requiredLockTime,
	}

	if len(condBlob) > 0 {
		items, condErr := decodeConditionWitness(condBlob)
		if condErr != nil {
			return nil, condErr
		}

		snap.ConditionWitness = items
	}

	if len(externalSigBlob) > 0 {
		sigs, sigErr := decodeExternalSignatures(externalSigBlob)
		if sigErr != nil {
			return nil, sigErr
		}

		snap.ExternalSignatures = sigs
	}

	return snap, nil
}

const (
	// maxConditionWitnessItems caps the number of witness stack
	// elements carried in a persisted TransferInputSnapshot. Ark's
	// custom spend paths (vHTLC claim/refund) use at most a
	// handful of condition items; the cap is generous but bounds
	// memory allocation when decoding an attacker-controlled or
	// corrupted durable blob.
	maxConditionWitnessItems = 64

	// maxConditionWitnessItemBytes caps the size of each individual
	// witness stack element. Bitcoin's consensus limit on standard
	// witness elements is 520 bytes (MAX_SCRIPT_ELEMENT_SIZE); we
	// apply the same limit here so a persisted condition witness
	// cannot hold any value that could not actually make it onto
	// the chain.
	maxConditionWitnessItemBytes = 520

	// maxExternalSignatures caps additional signatures carried by one
	// custom input. Current vHTLC refunds need one external swap-server
	// signature.
	maxExternalSignatures = 8

	// maxExternalSignatureScriptBytes caps the witness script copied into
	// each external signature record.
	maxExternalSignatureScriptBytes = 520

	// maxExternalSignatureBytes caps Schnorr signatures plus optional
	// sighash byte.
	maxExternalSignatureBytes = 65
)

// encodeConditionWitness serializes a list of witness items as
// count-prefixed length-prefixed byte strings. Enforces the same caps
// as the decoder so in-memory state and on-disk state can't drift.
func encodeConditionWitness(items [][]byte) ([]byte, error) {
	if len(items) > maxConditionWitnessItems {
		return nil, fmt.Errorf("condition witness item count %d "+
			"exceeds maximum %d", len(items),
			maxConditionWitnessItems)
	}

	var buf bytes.Buffer

	if err := wire.WriteVarInt(&buf, 0, uint64(len(items))); err != nil {
		return nil, err
	}

	for i, item := range items {
		if len(item) > maxConditionWitnessItemBytes {
			return nil, fmt.Errorf("condition witness item %d "+
				"size %d exceeds maximum %d", i, len(item),
				maxConditionWitnessItemBytes)
		}

		if err := wire.WriteVarBytes(&buf, 0, item); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// decodeConditionWitness deserializes a list of witness items,
// bounding both the item count and the per-item size so a crafted or
// corrupted durable blob cannot force multi-GB allocations.
func decodeConditionWitness(raw []byte) ([][]byte, error) {
	r := bytes.NewReader(raw)

	count, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, err
	}

	if count > maxConditionWitnessItems {
		return nil, fmt.Errorf("condition witness item count %d "+
			"exceeds maximum %d", count, maxConditionWitnessItems)
	}

	items := make([][]byte, count)
	for i := uint64(0); i < count; i++ {
		item, readErr := wire.ReadVarBytes(
			r, 0, maxConditionWitnessItemBytes,
			"condition witness item",
		)
		if readErr != nil {
			return nil, readErr
		}

		items[i] = item
	}

	return items, nil
}

// encodeExternalSignatures serializes pre-collected tapscript signatures into a
// bounded durable blob.
func encodeExternalSignatures(sigs []ExternalTaprootScriptSignature) ([]byte,
	error) {

	if len(sigs) > maxExternalSignatures {
		return nil, fmt.Errorf("external signature count %d exceeds "+
			"maximum %d", len(sigs), maxExternalSignatures)
	}

	var buf bytes.Buffer
	if err := wire.WriteVarInt(&buf, 0, uint64(len(sigs))); err != nil {
		return nil, err
	}

	for i := range sigs {
		sig := sigs[i]
		if sig.PubKey == nil {
			return nil, fmt.Errorf("external signature %d pubkey "+
				"is required", i)
		}

		if len(sig.WitnessScript) > maxExternalSignatureScriptBytes {
			return nil, fmt.Errorf("external signature %d witness "+
				"script size %d exceeds maximum %d", i,
				len(sig.WitnessScript),
				maxExternalSignatureScriptBytes)
		}

		if len(sig.Signature) > maxExternalSignatureBytes {
			return nil, fmt.Errorf("external signature %d size %d "+
				"exceeds maximum %d", i, len(sig.Signature),
				maxExternalSignatureBytes)
		}

		if err := wire.WriteVarBytes(
			&buf, 0, sig.PubKey.SerializeCompressed(),
		); err != nil {
			return nil, err
		}

		if err := wire.WriteVarBytes(
			&buf, 0, sig.WitnessScript,
		); err != nil {
			return nil, err
		}

		if err := wire.WriteVarBytes(
			&buf, 0, sig.Signature,
		); err != nil {
			return nil, err
		}

		if err := binary.Write(
			&buf, binary.BigEndian, uint32(sig.SigHash),
		); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// decodeExternalSignatures deserializes the bounded durable external signature
// blob.
func decodeExternalSignatures(raw []byte) ([]ExternalTaprootScriptSignature,
	error) {

	r := bytes.NewReader(raw)
	count, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, err
	}

	if count > maxExternalSignatures {
		return nil, fmt.Errorf("external signature count %d exceeds "+
			"maximum %d", count, maxExternalSignatures)
	}

	result := make([]ExternalTaprootScriptSignature, 0, count)
	for i := uint64(0); i < count; i++ {
		pubKeyBytes, err := wire.ReadVarBytes(
			r, 0, 33, "external signature pubkey",
		)
		if err != nil {
			return nil, err
		}

		pubKey, err := btcec.ParsePubKey(pubKeyBytes)
		if err != nil {
			return nil, fmt.Errorf("parse external signature "+
				"pubkey %d: %w", i, err)
		}

		witnessScript, err := wire.ReadVarBytes(
			r, 0, maxExternalSignatureScriptBytes,
			"external signature witness script",
		)
		if err != nil {
			return nil, err
		}

		signature, err := wire.ReadVarBytes(
			r, 0, maxExternalSignatureBytes, "external signature",
		)
		if err != nil {
			return nil, err
		}

		var sigHash uint32
		if err := binary.Read(
			r, binary.BigEndian, &sigHash,
		); err != nil {
			return nil, err
		}

		result = append(result, ExternalTaprootScriptSignature{
			PubKey:        pubKey,
			WitnessScript: witnessScript,
			Signature:     signature,
			SigHash:       txscript.SigHashType(sigHash),
		})
	}

	if r.Len() != 0 {
		return nil, fmt.Errorf("external signatures contain trailing " +
			"bytes")
	}

	return result, nil
}

func encodeSessionPayload(sessionID SessionID) ([]byte, error) {
	sessionBytes := sessionIDBytes(sessionID)
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			sessionPayloadSessionIDRecordType, &sessionBytes,
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

func decodeSessionPayload(raw []byte) (SessionID, error) {
	var sessionBytes []byte
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			sessionPayloadSessionIDRecordType, &sessionBytes,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return SessionID{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return SessionID{}, err
	}

	return parseSessionID(sessionBytes)
}

// encodeResumePayload encodes a resume request's session id alongside the
// flag distinguishing a fired retry timer from a boot restore. The flag rides
// on the durable payload so a crash between the timer firing and the child's
// turn replays with the same give-up semantics.
func encodeResumePayload(sessionID SessionID,
	fromRetryTimer bool) ([]byte, error) {

	sessionBytes := sessionIDBytes(sessionID)
	var timer uint8
	if fromRetryTimer {
		timer = 1
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			resumePayloadSessionIDRecordType, &sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			resumePayloadFromRetryTimerRecordType, &timer,
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

// decodeResumePayload decodes a resume request payload. The retry-timer flag is
// optional so a legacy session-only payload decodes as a boot restore.
func decodeResumePayload(raw []byte) (SessionID, bool, error) {
	var (
		sessionBytes []byte
		timer        uint8
	)
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			resumePayloadSessionIDRecordType, &sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			resumePayloadFromRetryTimerRecordType, &timer,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return SessionID{}, false, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return SessionID{}, false, err
	}

	sessionID, err := parseSessionID(sessionBytes)
	if err != nil {
		return SessionID{}, false, err
	}

	return sessionID, timer == 1, nil
}

// encodeListSessionsPayload encodes the durable list-sessions query filters.
func encodeListSessionsPayload(direction SessionDirection,
	pendingOnly bool) ([]byte, error) {

	dir := uint8(direction)
	var pending uint8
	if pendingOnly {
		pending = 1
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			listSessionsDirectionRecordType, &dir,
		),
		tlv.MakePrimitiveRecord(
			listSessionsPendingOnlyRecordType, &pending,
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

// decodeListSessionsPayload decodes the durable list-sessions query filters.
func decodeListSessionsPayload(raw []byte) (SessionDirection, bool, error) {
	var (
		dir     uint8
		pending uint8
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			listSessionsDirectionRecordType, &dir,
		),
		tlv.MakePrimitiveRecord(
			listSessionsPendingOnlyRecordType, &pending,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return SessionDirectionAll, false, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return SessionDirectionAll, false, err
	}

	if pending > 1 {
		return SessionDirectionAll, false, fmt.Errorf("pending-only " +
			"flag must be 0 or 1")
	}

	direction := SessionDirection(dir)
	switch direction {
	case SessionDirectionAll, SessionDirectionOutgoing,
		SessionDirectionIncoming:

	default:
		return SessionDirectionAll, false, fmt.Errorf("unknown "+
			"session direction: %d", dir)
	}

	return direction, pending == 1, nil
}

func encodeResolveIncomingTransferPayload(sessionID SessionID,
	recipientPkScript []byte, recipientEventID uint64) ([]byte, error) {

	sessionBytes := sessionIDBytes(sessionID)
	pkScript := recipientPkScript
	eventID := recipientEventID

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			resolveIncomingPayloadSessionIDRecordType,
			&sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			resolveIncomingPayloadPkScriptRecordType, &pkScript,
		),
		tlv.MakePrimitiveRecord(
			resolveIncomingPayloadEventIDRecordType, &eventID,
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

// decodeResolveIncomingTransferPayloadWithLimits decodes an incoming-transfer
// hint while enforcing the configured mailbox script byte limit.
func decodeResolveIncomingTransferPayloadWithLimits(raw []byte,
	limits ReceiveLimits) (SessionID, []byte, uint64, error) {

	var (
		sessionBytes      []byte
		recipientPkScript []byte
		recipientEventID  uint64
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			resolveIncomingPayloadSessionIDRecordType,
			&sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			resolveIncomingPayloadPkScriptRecordType,
			&recipientPkScript,
		),
		tlv.MakePrimitiveRecord(
			resolveIncomingPayloadEventIDRecordType,
			&recipientEventID,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return SessionID{}, nil, 0, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return SessionID{}, nil, 0, err
	}

	sessionID, err := parseSessionID(sessionBytes)
	if err != nil {
		return SessionID{}, nil, 0, err
	}

	limits = normalizeReceiveLimits(limits)
	if uint64(len(recipientPkScript)) >
		uint64(limits.MaxMailboxScriptBytes) {
		return SessionID{}, nil, 0, fmt.Errorf("max mailbox script "+
			"bytes exceeded: recipient pk_script length %d "+
			"exceeds limit %d", len(recipientPkScript),
			limits.MaxMailboxScriptBytes)
	}

	return sessionID, recipientPkScript, recipientEventID, nil
}

func encodeRestoreSnapshotPayload(snapshot *OutgoingSnapshot) ([]byte, error) {
	snapshotRaw, err := encodeOutgoingSnapshot(snapshot)
	if err != nil {
		return nil, err
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			restorePayloadSnapshotRecordType, &snapshotRaw,
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

func decodeRestoreSnapshotPayload(raw []byte) (*OutgoingSnapshot, error) {
	return decodeRestoreSnapshotPayloadWithLimits(
		raw, ReceiveLimits{},
	)
}

// decodeRestoreSnapshotPayloadWithLimits decodes a restore payload and applies
// receive limits to the embedded snapshot.
func decodeRestoreSnapshotPayloadWithLimits(raw []byte,
	limits ReceiveLimits) (*OutgoingSnapshot, error) {

	var snapshotRaw []byte
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			restorePayloadSnapshotRecordType, &snapshotRaw,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return nil, err
	}

	return decodeOutgoingSnapshotWithLimits(snapshotRaw, limits)
}

func encodeDriveEventRequestPayload(sessionID SessionID,
	event Event) ([]byte, error) {

	if event == nil {
		return nil, fmt.Errorf("event must be provided")
	}

	// Validation of SubmitAcceptedEvent identity (ArkPSBT, session ID
	// match) is deferred to the processing layer (handleDriveEvent)
	// rather than the serialization layer. This allows server-push
	// events dispatched via the EventRouter to be persisted to the
	// durable mailbox with nil ArkPSBT, which the actor enriches from
	// session state before validation.

	sessionBytes := sessionIDBytes(sessionID)
	eventPayload, err := encodeEventPayload(event)
	if err != nil {
		return nil, err
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			driveEventPayloadSessionIDRecordType, &sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			driveEventPayloadEventRecordType, &eventPayload,
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

// decodeDriveEventRequestPayloadWithLimits decodes a drive-event request and
// applies receive limits to the embedded event payload.
func decodeDriveEventRequestPayloadWithLimits(raw []byte,
	limits ReceiveLimits) (SessionID, Event, error) {

	var (
		sessionBytes []byte
		eventPayload []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			driveEventPayloadSessionIDRecordType, &sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			driveEventPayloadEventRecordType, &eventPayload,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return SessionID{}, nil, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return SessionID{}, nil, err
	}

	sessionID, err := parseSessionID(sessionBytes)
	if err != nil {
		return SessionID{}, nil, err
	}

	event, err := decodeEventPayloadWithLimits(eventPayload, limits)
	if err != nil {
		return SessionID{}, nil, err
	}

	if incoming, ok := event.(*IncomingTransferEvent); ok {
		incoming.SessionID = sessionID
	}

	// Validation of SubmitAcceptedEvent identity is deferred to the
	// processing layer (handleDriveEvent), not the deserialization
	// layer. See encodeDriveEventRequestPayload for rationale.

	return sessionID, event, nil
}

//nolint:funlen
func encodeEventPayload(event Event) ([]byte, error) {
	var (
		eventKind        uint64
		submitSession    []byte
		arkPSBT          []byte
		checkpointPSBT   []byte
		reason           []byte
		outpointPayload  []byte
		metadataPayload  []byte
		ancestorPayload  []byte
		outboxType       []byte
		retryable        uint8
		retryAfterNanos  uint64
		err              error
		recipientPayload []byte
	)

	switch evt := event.(type) {
	case *ArkSignedEvent:
		eventKind = eventKindArkSigned
		if evt.ArkPSBT == nil {
			return nil, fmt.Errorf("ark signed event psbt required")
		}

		arkPSBT, err = psbtutil.Serialize(evt.ArkPSBT)
		if err != nil {
			return nil, err
		}

	case *SubmitAcceptedEvent:
		eventKind = eventKindSubmitAccepted
		submitSession = sessionIDBytes(evt.SessionID)

		// ArkPSBT may be nil for server-push events dispatched
		// via the EventRouter. The actor layer enriches it from
		// session state before processing. Encode empty bytes
		// when nil so the TLV stream is well-formed.
		if evt.ArkPSBT != nil {
			arkPSBT, err = psbtutil.Serialize(evt.ArkPSBT)
			if err != nil {
				return nil, err
			}
		}

		checkpoints, err := serializePSBTSlice(
			evt.CoSignedCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}
		checkpointPSBT, err = encodeLengthPrefixedBlobList(checkpoints)
		if err != nil {
			return nil, err
		}

	case *CheckpointsSignedEvent:
		eventKind = eventKindCheckpointsSigned
		checkpoints, err := serializePSBTSlice(evt.FinalCheckpointPSBTs)
		if err != nil {
			return nil, err
		}
		checkpointPSBT, err = encodeLengthPrefixedBlobList(checkpoints)
		if err != nil {
			return nil, err
		}

	case *FinalizeAcceptedEvent:
		eventKind = eventKindFinalizeAccepted

	case *InputsMarkedSpentEvent:
		eventKind = eventKindInputsMarkedSpent

	case *FailEvent:
		eventKind = eventKindFail
		reason = []byte(evt.Reason)

	case *IncomingTransferEvent:
		eventKind = eventKindIncomingTransfer

		if evt.ArkPSBT == nil {
			return nil, fmt.Errorf("incoming transfer event ark " +
				"psbt must be provided")
		}

		arkPSBT, err = psbtutil.Serialize(evt.ArkPSBT)
		if err != nil {
			return nil, err
		}

		checkpoints, err := serializePSBTSlice(
			evt.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}

		checkpointPSBT, err = encodeLengthPrefixedBlobList(
			checkpoints,
		)
		if err != nil {
			return nil, err
		}

		ancestorRaw, err := encodePackageArtifacts(
			evt.AncestorPackages,
		)
		if err != nil {
			return nil, err
		}
		ancestorPayload = ancestorRaw

		recipientPayload, err = encodeIncomingRecipients(
			evt.Recipients,
		)
		if err != nil {
			return nil, err
		}

	case *IncomingHandledEvent:
		eventKind = eventKindIncomingHandled

		outpoints := evt.MaterializedOutpoints
		if len(outpoints) == 0 && len(evt.MaterializedVTXOs) > 0 {
			outpoints = make(
				[]wire.OutPoint, 0, len(evt.MaterializedVTXOs),
			)
			for _, desc := range evt.MaterializedVTXOs {
				if desc == nil {
					continue
				}

				outpoints = append(outpoints, desc.Outpoint)
			}
		}

		outpointPayload, err = encodeOutPointList(outpoints)
		if err != nil {
			return nil, err
		}

	case *IncomingMetadataResolvedEvent:
		eventKind = eventKindIncomingMetadata

		metadataPayload, err = encodeIncomingMetadataMatches(
			evt.Matches,
		)
		if err != nil {
			return nil, err
		}

	case *IncomingAckSentEvent:
		eventKind = eventKindIncomingAckSent

	case *OutboxErrorEvent:
		eventKind = eventKindOutboxError
		outboxType = []byte(evt.OutboxType)
		if evt.Retryable {
			retryable = 1
		}
		retryAfterNanos = uint64(evt.RetryAfter)
		reason = []byte(evt.ErrorReason)

	default:
		return nil, fmt.Errorf("unsupported event type: %T", event)
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(eventPayloadKindRecordType, &eventKind),
		tlv.MakePrimitiveRecord(
			eventPayloadSubmitSessionIDRecordType, &submitSession,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadArkPSBTRecordType, &arkPSBT,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadCheckpointPSBTsRecordType, &checkpointPSBT,
		),
		tlv.MakePrimitiveRecord(eventPayloadReasonRecordType, &reason),
		tlv.MakePrimitiveRecord(
			eventPayloadOutpointsRecordType, &outpointPayload,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadMetadataMatchesRecordType, &metadataPayload,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadAncestorPackagesRecordType,
			&ancestorPayload,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadOutboxTypeRecordType, &outboxType,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadRetryableRecordType, &retryable,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadRetryAfterNanosRecordType, &retryAfterNanos,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadIncomingRecipientsType, &recipientPayload,
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

// decodeEventPayloadWithLimits decodes an event payload and applies receive
// limits to nested list-shaped event fields.
func decodeEventPayloadWithLimits(raw []byte,
	limits ReceiveLimits) (Event, error) {

	var (
		eventKind        uint64
		submitSession    []byte
		arkPSBT          []byte
		checkpointPSBT   []byte
		reason           []byte
		outpointPayload  []byte
		metadataPayload  []byte
		ancestorPayload  []byte
		outboxType       []byte
		retryable        uint8
		retryAfterNanos  uint64
		recipientPayload []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(eventPayloadKindRecordType, &eventKind),
		tlv.MakePrimitiveRecord(
			eventPayloadSubmitSessionIDRecordType, &submitSession,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadArkPSBTRecordType, &arkPSBT,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadCheckpointPSBTsRecordType, &checkpointPSBT,
		),
		tlv.MakePrimitiveRecord(eventPayloadReasonRecordType, &reason),
		tlv.MakePrimitiveRecord(
			eventPayloadOutpointsRecordType, &outpointPayload,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadMetadataMatchesRecordType, &metadataPayload,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadAncestorPackagesRecordType,
			&ancestorPayload,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadOutboxTypeRecordType, &outboxType,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadRetryableRecordType, &retryable,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadRetryAfterNanosRecordType, &retryAfterNanos,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadIncomingRecipientsType, &recipientPayload,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return nil, err
	}

	switch eventKind {
	case eventKindArkSigned:
		ark, err := psbtutil.Parse(arkPSBT)
		if err != nil {
			return nil, err
		}

		return &ArkSignedEvent{ArkPSBT: ark}, nil

	case eventKindSubmitAccepted:
		sessionID, err := parseSessionID(submitSession)
		if err != nil {
			return nil, err
		}

		// ArkPSBT may be empty for server-push events where the
		// proto response does not echo it back. The actor layer
		// enriches it from session state before processing.
		var ark *psbt.Packet
		if len(arkPSBT) > 0 {
			ark, err = psbtutil.Parse(arkPSBT)
			if err != nil {
				return nil, err
			}
		}

		checkpoints, err := decodeCheckpointPSBTsWithLimits(
			checkpointPSBT, limits,
		)
		if err != nil {
			return nil, err
		}

		return &SubmitAcceptedEvent{
			SessionID:               sessionID,
			ArkPSBT:                 ark,
			CoSignedCheckpointPSBTs: checkpoints,
		}, nil

	case eventKindCheckpointsSigned:
		checkpoints, err := decodeCheckpointPSBTsWithLimits(
			checkpointPSBT, limits,
		)
		if err != nil {
			return nil, err
		}

		return &CheckpointsSignedEvent{
			FinalCheckpointPSBTs: checkpoints,
		}, nil

	case eventKindFinalizeAccepted:
		return &FinalizeAcceptedEvent{}, nil

	case eventKindInputsMarkedSpent:
		return &InputsMarkedSpentEvent{}, nil

	case eventKindFail:
		return &FailEvent{Reason: string(reason)}, nil

	case eventKindIncomingTransfer:
		if len(arkPSBT) == 0 {
			return nil, fmt.Errorf("incoming transfer event ark " +
				"psbt must be provided")
		}

		ark, err := psbtutil.Parse(arkPSBT)
		if err != nil {
			return nil, err
		}

		checkpoints, err := decodeCheckpointPSBTsWithLimits(
			checkpointPSBT, limits,
		)
		if err != nil {
			return nil, err
		}

		ancestors, err := decodePackageArtifactsWithLimits(
			ancestorPayload, limits,
		)
		if err != nil {
			return nil, err
		}

		recipients, err := decodeIncomingRecipientsWithLimits(
			recipientPayload, limits,
		)
		if err != nil {
			return nil, err
		}

		return &IncomingTransferEvent{
			ArkPSBT:              ark,
			FinalCheckpointPSBTs: checkpoints,
			AncestorPackages:     ancestors,
			Recipients:           recipients,
		}, nil

	case eventKindIncomingHandled:
		outpoints, err := decodeOutPointListWithLimits(
			outpointPayload, limits,
		)
		if err != nil {
			return nil, err
		}

		return &IncomingHandledEvent{
			MaterializedOutpoints: outpoints,
		}, nil

	case eventKindIncomingMetadata:
		matches, err := decodeIncomingMetadataMatchesWithLimits(
			metadataPayload, limits,
		)
		if err != nil {
			return nil, err
		}

		return &IncomingMetadataResolvedEvent{
			Matches: matches,
		}, nil

	case eventKindIncomingAckSent:
		return &IncomingAckSentEvent{}, nil

	case eventKindOutboxError:
		return &OutboxErrorEvent{
			OutboxType:  string(outboxType),
			Retryable:   retryable != 0,
			RetryAfter:  time.Duration(retryAfterNanos),
			ErrorReason: string(reason),
		}, nil

	default:
		return nil, fmt.Errorf("unknown event kind: %d", eventKind)
	}
}

// decodeCheckpointPSBTsWithLimits decodes checkpoint PSBT lists using receive
// limits for the outer blob list.
func decodeCheckpointPSBTsWithLimits(raw []byte,
	limits ReceiveLimits) ([]*psbt.Packet, error) {

	checkpointRaw, err := decodeLengthPrefixedBlobListWithLimits(
		raw, limits,
	)
	if err != nil {
		return nil, err
	}

	return parsePSBTSlice(checkpointRaw)
}

func validateSubmitAcceptedIdentity(sessionID SessionID,
	event *SubmitAcceptedEvent) error {

	if event == nil {
		return fmt.Errorf("submit accepted event must be provided")
	}

	if event.SessionID != sessionID {
		return fmt.Errorf("submit accepted event session id mismatch")
	}

	if event.ArkPSBT == nil || event.ArkPSBT.UnsignedTx == nil {
		return fmt.Errorf("submit accepted event ark psbt must be " +
			"provided")
	}

	arkSessionID, err := sessionIDFromArk(event.ArkPSBT)
	if err != nil {
		return err
	}

	if arkSessionID != sessionID {
		return fmt.Errorf("submit accepted event ark txid mismatch")
	}

	return nil
}

func outPointBytes(out wire.OutPoint) []byte {
	raw := make([]byte, chainhash.HashSize+4)
	copy(raw[:chainhash.HashSize], out.Hash[:])
	binary.LittleEndian.PutUint32(raw[chainhash.HashSize:], out.Index)

	return raw
}

func parseOutPointBytes(raw []byte) (wire.OutPoint, error) {
	if len(raw) != chainhash.HashSize+4 {
		return wire.OutPoint{}, fmt.Errorf("invalid outpoint length")
	}

	var out wire.OutPoint
	copy(out.Hash[:], raw[:chainhash.HashSize])
	out.Index = binary.LittleEndian.Uint32(raw[chainhash.HashSize:])

	return out, nil
}

func encodeLengthPrefixedBlobList(blobs [][]byte) ([]byte, error) {
	var (
		buf     bytes.Buffer
		scratch [8]byte
	)

	if err := tlv.WriteVarInt(
		&buf, uint64(len(blobs)), &scratch,
	); err != nil {
		return nil, err
	}

	for i := range blobs {
		element := blobs[i]

		if err := tlv.WriteVarInt(
			&buf,
			uint64(
				len(element),
			),
			&scratch,
		); err != nil {
			return nil, err
		}

		if _, err := buf.Write(element); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

func decodeLengthPrefixedBlobList(raw []byte) ([][]byte, error) {
	return decodeLengthPrefixedBlobListWithLimits(
		raw, ReceiveLimits{},
	)
}

// decodeLengthPrefixedBlobListWithLimits decodes a blob list after enforcing
// the configured item-count cap before allocating the output slice.
func decodeLengthPrefixedBlobListWithLimits(raw []byte,
	limits ReceiveLimits) ([][]byte, error) {

	var scratch [8]byte

	reader := bytes.NewReader(raw)
	count, err := tlv.ReadVarInt(reader, &scratch)
	if err != nil {
		return nil, err
	}

	limits = normalizeReceiveLimits(limits)
	if count > uint64(limits.MaxMailboxItems) {
		return nil, fmt.Errorf("max mailbox items exceeded: blob list "+
			"count %d exceeds limit %d", count,
			limits.MaxMailboxItems)
	}

	blobs := make([][]byte, 0, count)
	for i := uint64(0); i < count; i++ {
		elementLen, err := tlv.ReadVarInt(reader, &scratch)
		if err != nil {
			return nil, err
		}

		element := make([]byte, elementLen)
		if _, err := io.ReadFull(reader, element); err != nil {
			return nil, err
		}

		blobs = append(blobs, element)
	}

	if reader.Len() != 0 {
		return nil, fmt.Errorf("trailing payload bytes")
	}

	return blobs, nil
}

func sessionIDBytes(sessionID SessionID) []byte {
	h := [32]byte(sessionID)
	raw := make([]byte, len(h))
	copy(raw, h[:])

	return raw
}

func parseSessionID(raw []byte) (SessionID, error) {
	if len(raw) != chainhash.HashSize {
		return SessionID{}, fmt.Errorf("invalid session id length")
	}

	var hash chainhash.Hash
	copy(hash[:], raw)

	return SessionID(hash), nil
}

func uint64ToInt64(value uint64, field string) (int64, error) {
	if value > math.MaxInt64 {
		return 0, fmt.Errorf("%s overflows int64: %d", field, value)
	}

	return int64(value), nil
}

func decodeUint64ToUint32(value uint64, field string) (uint32, error) {
	if value > math.MaxUint32 {
		return 0, fmt.Errorf("%s overflows uint32: %d", field, value)
	}

	return uint32(value), nil
}

func uint32ToInt32(value uint32, field string) (int32, error) {
	if value > math.MaxInt32 {
		return 0, fmt.Errorf("%s overflows int32: %d", field, value)
	}

	return int32(value), nil
}
