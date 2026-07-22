package serverconn

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/internal/indexerlimits"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightningnetwork/lnd/tlv"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	// SendListOORRecipientEventsByScriptRequestMsgType is the TLV
	// type for durable proof-gated indexer queries that resolve a
	// lightweight incoming OOR hint into the full recipient-event
	// package.
	SendListOORRecipientEventsByScriptRequestMsgType tlv.Type = 2003

	// SendListVTXOsByScriptsRequestMsgType is the TLV type for durable
	// proof-gated indexer queries that resolve authoritative incoming VTXO
	// metadata by taproot output script.
	SendListVTXOsByScriptsRequestMsgType tlv.Type = 2004
)

type (
	listRecipientPkScriptRecordTLV    = tlv.TlvType1
	listRecipientAfterEventRecordTLV  = tlv.TlvType2
	listRecipientLimitRecordTLV       = tlv.TlvType3
	listRecipientCorrelationRecordTLV = tlv.TlvType4
	listRecipientMsgIDRecordTLV       = tlv.TlvType5
	listRecipientIdempotencyRecordTLV = tlv.TlvType6
	listVTXOsPkScriptsRecordTLV       = tlv.TlvType1
	listVTXOsLegacyCursorRecordTLV    = tlv.TlvType2
	listVTXOsLimitRecordTLV           = tlv.TlvType3
	listVTXOsCorrelationRecordTLV     = tlv.TlvType4
	listVTXOsMsgIDRecordTLV           = tlv.TlvType5
	listVTXOsIdempotencyRecordTLV     = tlv.TlvType6
	listVTXOsAfterCursorRecordTLV     = tlv.TlvType7
)

// SendListOORRecipientEventsByScriptRequest describes a durable proof-gated
// indexer unary query for one taproot output script.
type SendListOORRecipientEventsByScriptRequest struct {
	actor.BaseMessage

	// PkScript is the taproot output script to query.
	PkScript []byte

	// AfterEventID is the exclusive lower bound for the recipient-event
	// stream cursor.
	AfterEventID uint64

	// Limit is the maximum number of recipient events to return.
	Limit uint32

	// CorrelationID links this request to the eventual KIND_RESPONSE.
	CorrelationID string

	// MsgID uniquely identifies this send attempt. Retries of the same
	// persisted request must reuse this ID.
	MsgID string

	// IdempotencyKey identifies the semantic operation for remote dedupe.
	IdempotencyKey string
}

// MessageType returns a human-readable type name for logging.
func (m *SendListOORRecipientEventsByScriptRequest) MessageType() string {
	return "SendListOORRecipientEventsByScriptRequest"
}

// TLVType returns the unique TLV type identifier for this message.
func (m *SendListOORRecipientEventsByScriptRequest) TLVType() tlv.Type {
	return SendListOORRecipientEventsByScriptRequestMsgType
}

// ServiceMethod returns the fixed mailbox route for this indexer unary.
func (m *SendListOORRecipientEventsByScriptRequest) ServiceMethod() mailboxrpc.ServiceMethod { //nolint:ll

	return mailboxrpc.ServiceMethod{
		Service: "arkrpc.IndexerService",
		Method:  "ListOORRecipientEventsByScript",
	}
}

// BuildBody constructs the proof-gated proto body and returns
// stable identity bytes for deterministic ID derivation.
func (m *SendListOORRecipientEventsByScriptRequest) BuildBody(
	ctx context.Context, builder DurableUnaryRequestBuilder) (*anypb.Any,
	[]byte, error) {

	protoReq, err := builder.
		BuildListOORRecipientEventsByScriptRequest(
			ctx, m.PkScript, m.AfterEventID, m.Limit,
		)
	if err != nil {
		return nil, nil, fmt.Errorf("build recipient-events "+
			"request: %w", err)
	}

	body, err := anypb.New(protoReq)
	if err != nil {
		return nil, nil, fmt.Errorf("wrap in Any: %w", err)
	}

	stable, err := encodeRecipientEventsQueryIdentity(
		m.PkScript, m.AfterEventID, m.Limit, m.CorrelationID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("encode identity: %w", err)
	}

	return body, stable, nil
}

// QueryCorrelationID returns the correlation ID.
func (m *SendListOORRecipientEventsByScriptRequest) QueryCorrelationID() string { //nolint:ll

	return m.CorrelationID
}

// QueryMsgID returns the caller-provided msg ID.
func (m *SendListOORRecipientEventsByScriptRequest) QueryMsgID() string { //nolint:ll

	return m.MsgID
}

// QueryIdempotencyKey returns the caller-provided idempotency
// key.
func (m *SendListOORRecipientEventsByScriptRequest) QueryIdempotencyKey() string { //nolint:ll

	return m.IdempotencyKey
}

// Encode serializes the message to the provided writer.
func (m *SendListOORRecipientEventsByScriptRequest) Encode(w io.Writer) error {
	stableBytes, err := encodeRecipientEventsQueryIdentity(
		m.PkScript, m.AfterEventID, m.Limit, m.CorrelationID,
	)
	if err != nil {
		return err
	}

	msgID := m.MsgID
	if msgID == "" {
		msgID = mailboxconn.StableEventMsgID(stableBytes)
	}

	idempotencyKey := m.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = mailboxconn.
			StableEventIdempotencyKey(
				stableBytes,
			)
	}

	pkScriptRec := tlv.NewPrimitiveRecord[listRecipientPkScriptRecordTLV](
		m.PkScript,
	)
	afterEventRec := tlv.NewPrimitiveRecord[listRecipientAfterEventRecordTLV]( //nolint:ll
		m.AfterEventID,
	)
	limit := uint64(m.Limit)
	limitRec := tlv.NewPrimitiveRecord[listRecipientLimitRecordTLV](
		limit,
	)
	correlationRec := tlv.NewPrimitiveRecord[listRecipientCorrelationRecordTLV]( //nolint:ll
		[]byte(m.CorrelationID),
	)
	msgIDRec := tlv.NewPrimitiveRecord[listRecipientMsgIDRecordTLV](
		[]byte(msgID),
	)
	idempotencyRec := tlv.NewPrimitiveRecord[listRecipientIdempotencyRecordTLV]( //nolint:ll
		[]byte(idempotencyKey),
	)

	stream, err := tlv.NewStream(
		pkScriptRec.Record(), afterEventRec.Record(), limitRec.Record(),
		correlationRec.Record(), msgIDRec.Record(),
		idempotencyRec.Record(),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the message from the provided reader.
func (m *SendListOORRecipientEventsByScriptRequest) Decode(r io.Reader) error {
	pkScriptRec := tlv.ZeroRecordT[listRecipientPkScriptRecordTLV, []byte]()
	afterEventRec := tlv.ZeroRecordT[
		listRecipientAfterEventRecordTLV,
		uint64,
	]()
	limitRec := tlv.ZeroRecordT[listRecipientLimitRecordTLV, uint64]()
	correlationRec := tlv.ZeroRecordT[
		listRecipientCorrelationRecordTLV,
		[]byte,
	]()
	msgIDRec := tlv.ZeroRecordT[listRecipientMsgIDRecordTLV, []byte]()
	idempotencyRec := tlv.ZeroRecordT[
		listRecipientIdempotencyRecordTLV,
		[]byte,
	]()

	stream, err := tlv.NewStream(
		pkScriptRec.Record(), afterEventRec.Record(), limitRec.Record(),
		correlationRec.Record(), msgIDRec.Record(),
		idempotencyRec.Record(),
	)
	if err != nil {
		return err
	}

	// Bound the untrusted payload before decode: this durable query
	// persists in the outbox and is replayed from disk, so a crafted
	// record length must not drive an unbounded make() in the tlv
	// library.
	safe, err := safeTLVReader(r)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(safe); err != nil {
		return err
	}

	m.PkScript = append([]byte(nil), pkScriptRec.Val...)
	m.AfterEventID = afterEventRec.Val
	if limitRec.Val > uint64(^uint32(0)) {
		return fmt.Errorf("recipient query limit overflows uint32: %d",
			limitRec.Val)
	}
	m.Limit = uint32(limitRec.Val)
	m.CorrelationID = string(correlationRec.Val)
	m.MsgID = string(msgIDRec.Val)
	m.IdempotencyKey = string(idempotencyRec.Val)

	return nil
}

// serverConnMsgSealed implements the ServerConnMsg interface seal.
func (m *SendListOORRecipientEventsByScriptRequest) serverConnMsgSealed() {}

// SendListVTXOsByScriptsRequest describes a durable proof-gated indexer unary
// query for one or more taproot output scripts.
type SendListVTXOsByScriptsRequest struct {
	actor.BaseMessage

	// PkScripts are the taproot output scripts to query.
	PkScripts [][]byte

	// AfterCursor is the exclusive lower bound for the VTXO query cursor.
	AfterCursor []byte

	// Limit is the maximum number of VTXOs to return.
	Limit uint32

	// CorrelationID links this request to the eventual KIND_RESPONSE.
	CorrelationID string

	// MsgID uniquely identifies this send attempt. Retries of the same
	// persisted request must reuse this ID.
	MsgID string

	// IdempotencyKey identifies the semantic operation for remote dedupe.
	IdempotencyKey string
}

// MessageType returns a human-readable type name for logging.
func (m *SendListVTXOsByScriptsRequest) MessageType() string {
	return "SendListVTXOsByScriptsRequest"
}

// TLVType returns the unique TLV type identifier for this message.
func (m *SendListVTXOsByScriptsRequest) TLVType() tlv.Type {
	return SendListVTXOsByScriptsRequestMsgType
}

// ServiceMethod returns the fixed mailbox route for this indexer unary.
func (m *SendListVTXOsByScriptsRequest) ServiceMethod() mailboxrpc.ServiceMethod { //nolint:ll

	return mailboxrpc.ServiceMethod{
		Service: "arkrpc.IndexerService",
		Method:  "ListVTXOsByScripts",
	}
}

// BuildBody constructs the proof-gated proto body and returns
// stable identity bytes for deterministic ID derivation.
func (m *SendListVTXOsByScriptsRequest) BuildBody(ctx context.Context,
	builder DurableUnaryRequestBuilder) (*anypb.Any, []byte, error) {

	protoReq, err := builder.BuildListVTXOsByScriptsRequest(
		ctx, m.PkScripts, m.AfterCursor, m.Limit,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build vtxo query: %w", err)
	}

	body, err := anypb.New(protoReq)
	if err != nil {
		return nil, nil, fmt.Errorf("wrap in Any: %w", err)
	}

	stable, err := encodeVTXOsByScriptsQueryIdentity(
		m.PkScripts, m.AfterCursor, m.Limit, m.CorrelationID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("encode identity: %w", err)
	}

	return body, stable, nil
}

// QueryCorrelationID returns the correlation ID.
func (m *SendListVTXOsByScriptsRequest) QueryCorrelationID() string {
	return m.CorrelationID
}

// QueryMsgID returns the caller-provided msg ID.
func (m *SendListVTXOsByScriptsRequest) QueryMsgID() string {
	return m.MsgID
}

// QueryIdempotencyKey returns the caller-provided idempotency
// key.
func (m *SendListVTXOsByScriptsRequest) QueryIdempotencyKey() string {
	return m.IdempotencyKey
}

// Encode serializes the message to the provided writer.
func (m *SendListVTXOsByScriptsRequest) Encode(w io.Writer) error {
	if err := indexerlimits.ValidateVTXOsByScriptsCursor(
		m.AfterCursor,
	); err != nil {
		return fmt.Errorf("after cursor: %w", err)
	}

	stableBytes, err := encodeVTXOsByScriptsQueryIdentity(
		m.PkScripts, m.AfterCursor, m.Limit, m.CorrelationID,
	)
	if err != nil {
		return err
	}

	msgID := m.MsgID
	if msgID == "" {
		msgID = mailboxconn.StableEventMsgID(stableBytes)
	}

	idempotencyKey := m.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = mailboxconn.
			StableEventIdempotencyKey(
				stableBytes,
			)
	}

	pkScriptsRaw, err := encodeLengthPrefixedBlobList(m.PkScripts)
	if err != nil {
		return err
	}

	pkScriptsRec := tlv.NewPrimitiveRecord[listVTXOsPkScriptsRecordTLV](
		pkScriptsRaw,
	)
	limit := uint64(m.Limit)
	limitRec := tlv.NewPrimitiveRecord[listVTXOsLimitRecordTLV](
		limit,
	)
	correlationRec := tlv.NewPrimitiveRecord[listVTXOsCorrelationRecordTLV](
		[]byte(m.CorrelationID),
	)
	msgIDRec := tlv.NewPrimitiveRecord[listVTXOsMsgIDRecordTLV](
		[]byte(msgID),
	)
	idempotencyRec := tlv.NewPrimitiveRecord[listVTXOsIdempotencyRecordTLV](
		[]byte(idempotencyKey),
	)
	afterCursorRec := tlv.NewPrimitiveRecord[listVTXOsAfterCursorRecordTLV](
		append(
			[]byte(nil), m.AfterCursor...,
		),
	)

	stream, err := tlv.NewStream(
		pkScriptsRec.Record(), limitRec.Record(),
		correlationRec.Record(), msgIDRec.Record(),
		idempotencyRec.Record(), afterCursorRec.Record(),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the message from the provided reader.
func (m *SendListVTXOsByScriptsRequest) Decode(r io.Reader) error {
	pkScriptsRec := tlv.ZeroRecordT[listVTXOsPkScriptsRecordTLV, []byte]()
	legacyCursorRec := tlv.ZeroRecordT[
		listVTXOsLegacyCursorRecordTLV,
		uint64,
	]()
	afterCursorRec := tlv.ZeroRecordT[
		listVTXOsAfterCursorRecordTLV,
		[]byte,
	]()
	limitRec := tlv.ZeroRecordT[listVTXOsLimitRecordTLV, uint64]()
	correlationRec := tlv.ZeroRecordT[
		listVTXOsCorrelationRecordTLV,
		[]byte,
	]()
	msgIDRec := tlv.ZeroRecordT[listVTXOsMsgIDRecordTLV, []byte]()
	idempotencyRec := tlv.ZeroRecordT[
		listVTXOsIdempotencyRecordTLV,
		[]byte,
	]()

	stream, err := tlv.NewStream(
		pkScriptsRec.Record(), legacyCursorRec.Record(),
		limitRec.Record(), correlationRec.Record(), msgIDRec.Record(),
		idempotencyRec.Record(), afterCursorRec.Record(),
	)
	if err != nil {
		return err
	}

	// Bound the untrusted payload before decode so a crafted record
	// length cannot drive an unbounded make() in the tlv library.
	safe, err := safeTLVReader(r)
	if err != nil {
		return err
	}

	parsed, err := stream.DecodeWithParsedTypes(safe)
	if err != nil {
		return err
	}

	pkScripts, err := decodeLengthPrefixedBlobList(pkScriptsRec.Val)
	if err != nil {
		return err
	}

	if limitRec.Val > uint64(^uint32(0)) {
		return fmt.Errorf("vtxo query limit overflows uint32: %d",
			limitRec.Val)
	}

	_, legacyCursorSet := parsed[legacyCursorRec.TlvType()]
	_, afterCursorSet := parsed[afterCursorRec.TlvType()]

	afterCursor, err := normalizeVTXOAfterCursor(
		legacyCursorRec.Val, legacyCursorSet, afterCursorRec.Val,
		afterCursorSet,
	)
	if err != nil {
		return err
	}

	m.PkScripts = pkScripts
	m.AfterCursor = afterCursor
	m.Limit = uint32(limitRec.Val)
	m.CorrelationID = string(correlationRec.Val)
	m.MsgID = string(msgIDRec.Val)
	m.IdempotencyKey = string(idempotencyRec.Val)

	return nil
}

// normalizeVTXOAfterCursor selects the decoded opaque cursor or translates an
// old uint64 cursor when replaying a durable query persisted before keyset
// cursors existed.
func normalizeVTXOAfterCursor(legacyCursor uint64, legacyCursorSet bool,
	afterCursor []byte, afterCursorSet bool) ([]byte, error) {

	if afterCursorSet {
		if err := indexerlimits.ValidateVTXOsByScriptsCursor(
			afterCursor,
		); err != nil {
			return nil, fmt.Errorf("after cursor: %w", err)
		}

		return append([]byte(nil), afterCursor...), nil
	}

	if legacyCursorSet {
		if legacyCursor != 0 {
			return nil, fmt.Errorf("unsupported legacy vtxo "+
				"cursor %d", legacyCursor)
		}

		return nil, nil
	}

	return nil, nil
}

// serverConnMsgSealed implements the ServerConnMsg interface seal.
func (m *SendListVTXOsByScriptsRequest) serverConnMsgSealed() {}

// encodeRecipientEventsQueryIdentity encodes the stable identity material for
// a recipient-events query.
func encodeRecipientEventsQueryIdentity(pkScript []byte, afterEventID uint64,
	limit uint32, correlationID string) ([]byte, error) {

	var buf bytes.Buffer

	if err := writeLengthPrefixedBlob(&buf, pkScript); err != nil {
		return nil, err
	}
	if err := binary.Write(
		&buf, binary.BigEndian, afterEventID,
	); err != nil {
		return nil, err
	}
	if err := binary.Write(
		&buf, binary.BigEndian, limit,
	); err != nil {
		return nil, err
	}
	if err := writeLengthPrefixedBlob(
		&buf, []byte(correlationID),
	); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodeVTXOsByScriptsQueryIdentity encodes the stable identity material for
// a VTXO-by-scripts query.
func encodeVTXOsByScriptsQueryIdentity(pkScripts [][]byte, afterCursor []byte,
	limit uint32, correlationID string) ([]byte, error) {

	var buf bytes.Buffer

	pkScriptsRaw, err := encodeLengthPrefixedBlobList(pkScripts)
	if err != nil {
		return nil, err
	}
	if err := writeLengthPrefixedBlob(&buf, pkScriptsRaw); err != nil {
		return nil, err
	}
	if err := writeLengthPrefixedBlob(&buf, afterCursor); err != nil {
		return nil, err
	}
	if err := binary.Write(
		&buf, binary.BigEndian, limit,
	); err != nil {
		return nil, err
	}
	if err := writeLengthPrefixedBlob(
		&buf, []byte(correlationID),
	); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodeLengthPrefixedBlobList encodes a blob list as a count-prefixed
// sequence of length-prefixed byte slices.
func encodeLengthPrefixedBlobList(blobs [][]byte) ([]byte, error) {
	var buf bytes.Buffer

	count := uint32(len(blobs))
	if err := binary.Write(
		&buf, binary.BigEndian, count,
	); err != nil {
		return nil, err
	}

	for i := range blobs {
		if err := writeLengthPrefixedBlob(&buf, blobs[i]); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// decodeLengthPrefixedBlobList decodes a blob list encoded by
// encodeLengthPrefixedBlobList.
func decodeLengthPrefixedBlobList(raw []byte) ([][]byte, error) {
	reader := bytes.NewReader(raw)

	var count uint32
	if err := binary.Read(
		reader, binary.BigEndian, &count,
	); err != nil {
		return nil, err
	}

	// Each blob carries at least its own 4-byte length prefix, so a
	// count larger than the remaining bytes can never be satisfied.
	// Bound it before make([][]byte, 0, count) so a crafted count (up
	// to 4 GiB for a uint32) cannot pre-allocate a multi-gigabyte
	// backing array and OOM the actor on replay.
	if uint64(count) > uint64(reader.Len())/4 {
		return nil, fmt.Errorf("blob count %d exceeds %d remaining "+
			"bytes", count, reader.Len())
	}

	blobs := make([][]byte, 0, count)
	for i := uint32(0); i < count; i++ {
		blob, err := readLengthPrefixedBlob(reader)
		if err != nil {
			return nil, err
		}

		blobs = append(blobs, blob)
	}

	if reader.Len() != 0 {
		return nil, fmt.Errorf("unexpected trailing bytes in blob list")
	}

	return blobs, nil
}

// writeLengthPrefixedBlob encodes one length-prefixed byte slice.
func writeLengthPrefixedBlob(w io.Writer, blob []byte) error {
	size := uint32(len(blob))
	if err := binary.Write(w, binary.BigEndian, size); err != nil {
		return err
	}

	_, err := w.Write(blob)

	return err
}

// readLengthPrefixedBlob decodes one length-prefixed byte slice. The
// declared size is bounded against the bytes physically remaining in
// the reader before make([]byte, size) so a crafted length (up to 4
// GiB for a uint32 prefix) cannot drive an OOM ahead of io.ReadFull.
// A blob can never legitimately carry more bytes than remain in its
// enclosing buffer.
func readLengthPrefixedBlob(r *bytes.Reader) ([]byte, error) {
	var size uint32
	if err := binary.Read(r, binary.BigEndian, &size); err != nil {
		return nil, err
	}

	if uint64(size) > uint64(r.Len()) {
		return nil, fmt.Errorf("blob length %d exceeds %d remaining "+
			"bytes", size, r.Len())
	}

	blob := make([]byte, size)
	if _, err := io.ReadFull(r, blob); err != nil {
		return nil, err
	}

	return blob, nil
}

// Compile-time interface checks.
var (
	_ DurableUnaryQuery = (*SendListOORRecipientEventsByScriptRequest)(nil)
	_ DurableUnaryQuery = (*SendListVTXOsByScriptsRequest)(nil)
)
