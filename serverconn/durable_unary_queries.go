package serverconn

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/internal/indexerlimits"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"google.golang.org/protobuf/types/known/anypb"
)

// SendListOORRecipientEventsByScriptRequest describes a proof-gated
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

// serverConnMsgSealed implements the ServerConnMsg interface seal.
func (m *SendListOORRecipientEventsByScriptRequest) serverConnMsgSealed() {}

// SendListVTXOsByScriptsRequest describes a proof-gated indexer unary
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

	if err := indexerlimits.ValidateVTXOsByScriptsCursor(
		m.AfterCursor,
	); err != nil {
		return nil, nil, fmt.Errorf("after cursor: %w", err)
	}

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

// writeLengthPrefixedBlob encodes one length-prefixed byte slice.
func writeLengthPrefixedBlob(w io.Writer, blob []byte) error {
	size := uint32(len(blob))
	if err := binary.Write(w, binary.BigEndian, size); err != nil {
		return err
	}

	_, err := w.Write(blob)

	return err
}

// Compile-time interface checks.
var (
	_ DurableUnaryQuery = (*SendListOORRecipientEventsByScriptRequest)(nil)
	_ DurableUnaryQuery = (*SendListVTXOsByScriptsRequest)(nil)
)
