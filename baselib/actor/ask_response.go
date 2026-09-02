package actor

import (
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// AskResponseMsgType is the TLV type identifier for AskResponse messages.
// This is a well-known type used by the DurableAsk pattern.
const AskResponseMsgType tlv.Type = 0xFFFF // 65535 - reserved for system messages

// TLV record type constants for AskResponse fields.
const (
	askResponseCorrelationIDType tlv.Type = 1
	askResponseResultBlobType    tlv.Type = 2
	askResponseErrorTextType     tlv.Type = 3
)

// AskResponse is a durable response message for DurableAsk requests.
// When an actor processes a message with callback metadata, it writes an
// AskResponse to its outbox targeting the callback actor. The OutboxPublisher
// then delivers this response to the caller's durable mailbox.
//
// This is the core mechanism for crash-safe Ask semantics: the response
// survives both caller and target crashes because it flows through the
// durable outbox/mailbox infrastructure.
//
// The ResultBlob contains a fully-encoded TLVMessage (with type ID prefix),
// allowing the caller to use their MessageCodec to decode the typed result.
// This enables generic AskResponse handling while preserving type safety.
type AskResponse struct {
	BaseMessage

	// CorrelationID links this response to the original DurableAsk request.
	// The caller uses this to match responses to pending requests.
	CorrelationID string

	// ResultBlob contains the codec-encoded result (includes TLV type ID).
	// Use DecodeResult() with a MessageCodec to get the typed result.
	// Empty if the request failed with an error.
	ResultBlob tlv.Blob

	// ErrorText contains the error message if the request failed.
	// Empty string if the request succeeded.
	ErrorText string
}

// MessageType returns a human-readable type name for logging.
func (m AskResponse) MessageType() string {
	return "actor.AskResponse"
}

// TLVType returns the unique TLV type identifier for this message.
func (m AskResponse) TLVType() tlv.Type {
	return AskResponseMsgType
}

// Encode serializes the message to the provided writer.
func (m AskResponse) Encode(w io.Writer) error {
	correlationID := []byte(m.CorrelationID)
	resultBlob := m.ResultBlob
	errorText := []byte(m.ErrorText)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			askResponseCorrelationIDType, &correlationID,
		),
		tlv.MakePrimitiveRecord(
			askResponseResultBlobType, &resultBlob,
		),
		tlv.MakePrimitiveRecord(
			askResponseErrorTextType, &errorText,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the message from the provided reader.
func (m *AskResponse) Decode(r io.Reader) error {
	var (
		correlationID []byte
		resultBlob    []byte
		errorText     []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			askResponseCorrelationIDType, &correlationID,
		),
		tlv.MakePrimitiveRecord(
			askResponseResultBlobType, &resultBlob,
		),
		tlv.MakePrimitiveRecord(
			askResponseErrorTextType, &errorText,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	// Bound the framing before decoding so a crafted durable payload cannot
	// drive an unbounded make() inside the tlv decoder (ResultBlob is a
	// []byte record sized from its declared length).
	safe, err := safeActorTLVReader(r)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(safe); err != nil {
		return err
	}

	m.CorrelationID = string(correlationID)
	m.ResultBlob = resultBlob
	m.ErrorText = string(errorText)

	return nil
}

// IsError returns true if this response represents an error.
func (m AskResponse) IsError() bool {
	return m.ErrorText != ""
}

// DecodeResult decodes the result blob using the provided codec.
// Returns an error if the response is an error or if decoding fails.
func (m AskResponse) DecodeResult(codec *MessageCodec) (TLVMessage, error) {
	if m.IsError() {
		return nil, fmt.Errorf("ask failed: %s", m.ErrorText)
	}

	if len(m.ResultBlob) == 0 {
		return nil, nil
	}

	return codec.Decode(m.ResultBlob)
}

// NewAskResponseSuccess creates a successful AskResponse with a raw result
// blob. Use NewAskResponseWithResult to encode a TLVMessage result.
func NewAskResponseSuccess(correlationID string,
	resultBlob tlv.Blob) *AskResponse {

	return &AskResponse{
		CorrelationID: correlationID,
		ResultBlob:    resultBlob,
		ErrorText:     "",
	}
}

// NewAskResponseWithResult creates a successful AskResponse by encoding the
// result using the provided codec. This is the preferred way to create
// responses as it ensures the result is properly encoded for decoding.
func NewAskResponseWithResult(correlationID string, codec *MessageCodec,
	result TLVMessage) (*AskResponse, error) {

	resultBlob, err := codec.Encode(result)
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}

	return &AskResponse{
		CorrelationID: correlationID,
		ResultBlob:    resultBlob,
		ErrorText:     "",
	}, nil
}

// NewAskResponseError creates an error AskResponse with the given error text.
func NewAskResponseError(correlationID string, errorText string) *AskResponse {
	return &AskResponse{
		CorrelationID: correlationID,
		ResultBlob:    nil,
		ErrorText:     errorText,
	}
}

// Compile-time interface check.
var _ TLVMessage = (*AskResponse)(nil)
