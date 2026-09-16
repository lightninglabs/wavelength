package round

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightningnetwork/lnd/tlv"
)

// clientStateRecord is the versioned, full-data checkpoint of a client FSM.
// The runtime consumes a mailbox message only after this record and its
// outgoing effects have committed together.
type clientStateRecord struct {
	Version           uint16
	Kind              uint8
	RoundID           []byte
	Intents           []byte
	Quote             []byte
	Commitment        []byte
	TxID              [32]byte
	Trees             []byte
	AssetLeaves       []byte
	TreeKey           []byte
	ConnectorKey      []byte
	SweepKey          []byte
	SweepDelay        uint32
	FlowVersion       uint32
	ForfeitKey        []byte
	ClientTrees       []byte
	BoardingIndices   []byte
	ForfeitMappings   []byte
	CollectedForfeits []byte
	AggNonces         []byte
	InputSigs         []byte
	Forfeited         []byte
	Failure           []byte
	Probes            uint32
	BlockHeight       uint32
	BlockHash         [32]byte
	Confirmations     uint32
	VTXOs             []byte
	RecoveryOutpoint  []byte
	SweepTxID         [32]byte
	Reason            []byte
	Cold              uint8
}

// stream binds the full snapshot to stable primitive TLV fields.
func (d *clientStateRecord) stream() (*tlv.Stream, error) {
	return tlv.NewStream(
		tlv.MakePrimitiveRecord(1, &d.Version),
		tlv.MakePrimitiveRecord(3, &d.Kind),
		tlv.MakePrimitiveRecord(5, &d.RoundID),
		tlv.MakePrimitiveRecord(7, &d.Intents),
		tlv.MakePrimitiveRecord(9, &d.Quote),
		tlv.MakePrimitiveRecord(11, &d.Commitment),
		tlv.MakePrimitiveRecord(13, &d.TxID),
		tlv.MakePrimitiveRecord(15, &d.Trees),
		tlv.MakePrimitiveRecord(17, &d.AssetLeaves),
		tlv.MakePrimitiveRecord(19, &d.TreeKey),
		tlv.MakePrimitiveRecord(21, &d.ConnectorKey),
		tlv.MakePrimitiveRecord(23, &d.SweepKey),
		tlv.MakePrimitiveRecord(25, &d.SweepDelay),
		tlv.MakePrimitiveRecord(27, &d.FlowVersion),
		tlv.MakePrimitiveRecord(29, &d.ForfeitKey),
		tlv.MakePrimitiveRecord(31, &d.ClientTrees),
		tlv.MakePrimitiveRecord(33, &d.BoardingIndices),
		tlv.MakePrimitiveRecord(35, &d.ForfeitMappings),
		tlv.MakePrimitiveRecord(37, &d.CollectedForfeits),
		tlv.MakePrimitiveRecord(39, &d.AggNonces),
		tlv.MakePrimitiveRecord(41, &d.InputSigs),
		tlv.MakePrimitiveRecord(43, &d.Forfeited),
		tlv.MakePrimitiveRecord(45, &d.Failure),
		tlv.MakePrimitiveRecord(47, &d.Probes),
		tlv.MakePrimitiveRecord(49, &d.BlockHeight),
		tlv.MakePrimitiveRecord(51, &d.BlockHash),
		tlv.MakePrimitiveRecord(53, &d.Confirmations),
		tlv.MakePrimitiveRecord(55, &d.VTXOs),
		tlv.MakePrimitiveRecord(57, &d.RecoveryOutpoint),
		tlv.MakePrimitiveRecord(59, &d.SweepTxID),
		tlv.MakePrimitiveRecord(61, &d.Reason),
		tlv.MakePrimitiveRecord(63, &d.Cold),
	)
}

// encodeClientState serializes all local state without making external calls.
func encodeClientState(state ClientState) ([]byte, error) {
	v, err := clientStateSnapshot(state)
	if err != nil {
		return nil, err
	}
	d := clientStateRecord{
		Version: 1,
		Kind:    v.kind,
		RoundID: v.roundID[:],
		TxID:    v.txID,
		TreeKey: durablePubKey(v.treeKey),
		ConnectorKey: durablePubKey(
			v.connectorKey,
		),
		SweepKey:    durablePubKey(v.sweepKey),
		SweepDelay:  v.sweepDelay,
		FlowVersion: uint32(v.flowVersion),
		ForfeitKey: durablePubKey(
			v.forfeitKey,
		),
		Probes:           v.probes,
		BlockHeight:      uint32(v.blockHeight),
		BlockHash:        v.blockHash,
		Confirmations:    uint32(v.confirmations),
		RecoveryOutpoint: durableOutpoint(v.recoveryOutpoint),
		SweepTxID:        v.sweepTxID,
		Reason:           []byte(v.reason),
	}
	if v.cold {
		d.Cold = 1
	}
	d.Intents, err = encodeDurableIntents(v.intents)
	if err != nil {
		return nil, fmt.Errorf("snapshot intents: %w", err)
	}
	d.Quote, err = encodeDurableQuote(v.roundID, v.quote)
	if err != nil {
		return nil, fmt.Errorf("snapshot quote: %w", err)
	}
	d.Commitment, err = encodeDurablePSBT(v.commitment)
	if err != nil {
		return nil, fmt.Errorf("snapshot commitment: %w", err)
	}
	d.Trees, err = encodeDurableMap(
		v.trees, encodeDurableIndex, encodeDurableTree,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot trees: %w", err)
	}
	d.AssetLeaves, err = encodeDurableMap(
		v.assetLeaves, encodeDurablePoint, durableBlob,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot assetLeaves: %w", err)
	}
	d.ClientTrees, err = encodeDurableMap(
		v.clientTrees, encodeDurableSignerKey, encodeDurableTree,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot clientTrees: %w", err)
	}
	d.BoardingIndices, err = encodeDurableMap(
		v.boardingIndices, encodeDurablePoint, encodeDurableIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot boardingIndices: %w", err)
	}
	d.ForfeitMappings, err = encodeDurableMap(
		v.forfeitMappings, encodeDurablePoint, encodeDurableConnector,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot forfeitMappings: %w", err)
	}
	d.CollectedForfeits, err = encodeDurableMap(
		v.collectedForfeits, encodeDurablePoint,
		encodeDurableForfeitResponse,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot collectedForfeits: %w", err)
	}
	d.AggNonces, err = encodeDurableMap(
		v.aggNonces, encodeDurableHash, encodeDurableNonce,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot aggNonces: %w", err)
	}
	d.InputSigs, err = encodeDurableList(
		v.inputSigs, encodeDurableInputSignature,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot inputSigs: %w", err)
	}
	d.Forfeited, err = encodeDurableList(v.forfeited, encodeDurablePoint)
	if err != nil {
		return nil, fmt.Errorf("snapshot forfeited: %w", err)
	}
	d.Failure, err = encodeDurableFailure(v.failure)
	if err != nil {
		return nil, fmt.Errorf("snapshot failure: %w", err)
	}
	d.VTXOs, err = encodeDurableList(v.vtxos, encodeDurableClientVTXO)
	if err != nil {
		return nil, fmt.Errorf("snapshot vtxos: %w", err)
	}
	stream, err := d.stream()
	if err != nil {
		return nil, err
	}
	var raw bytes.Buffer
	if err := stream.Encode(&raw); err != nil {
		return nil, err
	}

	return raw.Bytes(), nil
}

// decodeClientState reconstructs the full state and reports whether its
// external signer sessions were lost. Callers must reconcile a lost-session
// state before accepting another signing event for that attempt.
func decodeClientState(raw []byte, params *chaincfg.Params) (ClientState, bool,
	error) {

	var d clientStateRecord
	stream, err := d.stream()
	if err != nil {
		return nil, false, err
	}
	fields, err := stream.DecodeWithParsedTypes(bytes.NewReader(raw))
	if err != nil {
		return nil, false, err
	}
	for field := tlv.Type(1); field <= 63; field += 2 {
		if _, ok := fields[field]; !ok {
			return nil, false, fmt.Errorf("missing client state "+
				"field %d", field)
		}
	}
	if d.Version != 1 {
		return nil, false, fmt.Errorf("unknown client state version %d",
			d.Version)
	}
	if d.Cold > 1 {
		return nil, false, fmt.Errorf("invalid client state cold flag")
	}
	roundID, err := parseDurableRoundID(d.RoundID)
	if err != nil {
		return nil, false, err
	}
	v := clientStateValues{
		kind:       d.Kind,
		roundID:    roundID,
		txID:       d.TxID,
		sweepDelay: d.SweepDelay,
		flowVersion: roundpb.FlowVersion(
			d.FlowVersion,
		),
		probes:        d.Probes,
		blockHeight:   int32(d.BlockHeight),
		blockHash:     d.BlockHash,
		confirmations: int32(d.Confirmations),
		sweepTxID:     d.SweepTxID,
		reason:        string(d.Reason),
		cold:          d.Cold == 1,
	}
	v.intents, err = decodeDurableIntents(d.Intents, params)
	if err != nil {
		return nil, false, fmt.Errorf("restore intents: %w", err)
	}
	v.quote, err = decodeDurableQuote(v.roundID, d.Quote)
	if err != nil {
		return nil, false, fmt.Errorf("restore quote: %w", err)
	}
	v.commitment, err = decodeDurablePSBT(d.Commitment)
	if err != nil {
		return nil, false, fmt.Errorf("restore commitment: %w", err)
	}
	v.trees, err = decodeDurableMap(
		d.Trees, decodeDurableIndex, decodeDurableTree,
	)
	if err != nil {
		return nil, false, fmt.Errorf("restore trees: %w", err)
	}
	v.assetLeaves, err = decodeDurableMap(
		d.AssetLeaves, parseDurableOutpoint, durableBlob,
	)
	if err != nil {
		return nil, false, fmt.Errorf("restore assetLeaves: %w", err)
	}
	v.clientTrees, err = decodeDurableMap(
		d.ClientTrees, decodeDurableSignerKey, decodeDurableTree,
	)
	if err != nil {
		return nil, false, fmt.Errorf("restore clientTrees: %w", err)
	}
	v.boardingIndices, err = decodeDurableMap(
		d.BoardingIndices, parseDurableOutpoint, decodeDurableIndex,
	)
	if err != nil {
		return nil, false, fmt.Errorf("restore boardingIndices: %w",
			err)
	}
	v.forfeitMappings, err = decodeDurableMap(
		d.ForfeitMappings, parseDurableOutpoint, decodeDurableConnector,
	)
	if err != nil {
		return nil, false, fmt.Errorf("restore forfeitMappings: %w",
			err)
	}
	v.collectedForfeits, err = decodeDurableMap(
		d.CollectedForfeits, parseDurableOutpoint,
		decodeDurableForfeitResponse,
	)
	if err != nil {
		return nil, false, fmt.Errorf("restore collectedForfeits: %w",
			err)
	}
	v.aggNonces, err = decodeDurableMap(
		d.AggNonces, decodeDurableHash, decodeDurableNonce,
	)
	if err != nil {
		return nil, false, fmt.Errorf("restore aggNonces: %w", err)
	}
	v.inputSigs, err = decodeDurableList(
		d.InputSigs, decodeDurableInputSignature,
	)
	if err != nil {
		return nil, false, fmt.Errorf("restore inputSigs: %w", err)
	}
	v.forfeited, err = decodeDurableList(d.Forfeited, parseDurableOutpoint)
	if err != nil {
		return nil, false, fmt.Errorf("restore forfeited: %w", err)
	}
	v.failure, err = decodeDurableFailure(d.Failure)
	if err != nil {
		return nil, false, fmt.Errorf("restore failure: %w", err)
	}
	v.vtxos, err = decodeDurableList(d.VTXOs, decodeDurableClientVTXO)
	if err != nil {
		return nil, false, fmt.Errorf("restore vtxos: %w", err)
	}
	v.treeKey, err = parseDurablePubKey(d.TreeKey)
	if err != nil {
		return nil, false, err
	}
	v.connectorKey, err = parseDurablePubKey(d.ConnectorKey)
	if err != nil {
		return nil, false, err
	}
	v.sweepKey, err = parseDurablePubKey(d.SweepKey)
	if err != nil {
		return nil, false, err
	}
	v.forfeitKey, err = parseDurablePubKey(d.ForfeitKey)
	if err != nil {
		return nil, false, err
	}
	v.recoveryOutpoint, err = parseDurableOutpoint(d.RecoveryOutpoint)
	if err != nil {
		return nil, false, err
	}

	return v.clientState()
}

// durableBlob adapts an already-owned blob to the common collection codec.
func durableBlob(raw []byte) ([]byte, error) { return raw, nil }

// encodeDurableHash preserves a transaction hash used as a nonce-map key.
func encodeDurableHash(value chainhash.Hash) ([]byte, error) {
	return value[:], nil
}

// decodeDurableHash requires the complete transaction hash.
func decodeDurableHash(raw []byte) (chainhash.Hash, error) {
	var value chainhash.Hash
	if len(raw) != len(value) {
		return value, fmt.Errorf("invalid durable hash")
	}
	copy(value[:], raw)

	return value, nil
}

// encodeDurableNonce stores a public nonce, never signer-private nonce data.
func encodeDurableNonce(value tree.Musig2PubNonce) ([]byte, error) {
	return value[:], nil
}

// decodeDurableNonce requires both compressed public nonce points.
func decodeDurableNonce(raw []byte) (tree.Musig2PubNonce, error) {
	var value tree.Musig2PubNonce
	if len(raw) != len(value) {
		return value, fmt.Errorf("invalid durable public nonce")
	}
	copy(value[:], raw)

	return value, nil
}
