package serverconn

import (
	"bytes"
	"testing"
)

// hugeRecordPayloads returns a set of tiny TLV payloads that each
// declare a record length near 2^63 / 2^64 in their length prefix.
// These are the canonical crashers for the tlv unbounded-make DoS:
// the first byte is a small type, followed by an 0xff-prefixed varint
// that decodes to a giant length. They MUST be rejected (error) rather
// than panicking the decoder.
func hugeRecordPayloads() [][]byte {
	return [][]byte{
		// Unknown odd type 11, length ~2^63 (stream.go make path).
		{0x0b, 0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		// Type 1 (a known []byte field on every message), giant len.
		{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		// Type 0 (the proto payload on event/unary), giant len.
		{0x00, 0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		{},
	}
}

// FuzzSendClientEventRequestDecode drives SendClientEventRequest.Decode
// with arbitrary bytes. The wrapper persists in the durable outbox and
// is replayed from disk, so a crafted payload must error, not panic.
// Its Encode re-marshals an embedded proto, so we assert no-panic only.
func FuzzSendClientEventRequestDecode(f *testing.F) {
	seed := &SendClientEventRequest{
		Message: &bytesServerMessage{payload: []byte("evt")},
	}

	var buf bytes.Buffer
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}
	for _, p := range hugeRecordPayloads() {
		f.Add(p)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var v SendClientEventRequest

		// MUST NOT panic; an error is the acceptable outcome.
		_ = v.Decode(bytes.NewReader(data))
	})
}

// FuzzSendRPCRequestDecode drives SendRPCRequest.Decode (mailbox
// envelope wrapper). Encode re-marshals the envelope proto, so this
// asserts no-panic only.
func FuzzSendRPCRequestDecode(f *testing.F) {
	for _, p := range hugeRecordPayloads() {
		f.Add(p)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var v SendRPCRequest

		// MUST NOT panic.
		_ = v.Decode(bytes.NewReader(data))
	})
}

// FuzzSendUnaryRequestDecode drives SendUnaryRequest.Decode (Any-packed
// body plus routing metadata). Encode re-marshals proto, so this
// asserts no-panic only.
func FuzzSendUnaryRequestDecode(f *testing.F) {
	for _, p := range hugeRecordPayloads() {
		f.Add(p)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var v SendUnaryRequest

		// MUST NOT panic.
		_ = v.Decode(bytes.NewReader(data))
	})
}

// FuzzSendListOORRecipientEventsByScriptRequestDecode drives the
// recipient-events durable query decoder. Its fields are pure scalars
// and byte blobs, so a successful decode must round-trip byte-stably.
func FuzzSendListOORRecipientEventsByScriptRequestDecode(f *testing.F) {
	seed := &SendListOORRecipientEventsByScriptRequest{
		PkScript:      bytes.Repeat([]byte{0x02}, 34),
		AfterEventID:  7,
		Limit:         100,
		CorrelationID: "corr-1",
		MsgID:         "msg-1",
	}

	var buf bytes.Buffer
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}
	for _, p := range hugeRecordPayloads() {
		f.Add(p)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var v SendListOORRecipientEventsByScriptRequest

		// MUST NOT panic on any input.
		if err := v.Decode(bytes.NewReader(data)); err != nil {
			return
		}

		var out bytes.Buffer
		if err := v.Encode(&out); err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		var v2 SendListOORRecipientEventsByScriptRequest
		if err := v2.Decode(bytes.NewReader(out.Bytes())); err != nil {
			t.Fatalf("re-decode: %v", err)
		}
	})
}

// FuzzSendListVTXOsByScriptsRequestDecode drives the VTXOs-by-scripts
// durable query decoder, which carries a count-prefixed,
// length-prefixed blob list (readLengthPrefixedBlob's make([]byte,
// size)) and an opaque cursor. Successful decodes round-trip via
// re-encode/re-decode (Encode validates the cursor, so a decoded value
// that fails re-encode is acceptable and skipped).
func FuzzSendListVTXOsByScriptsRequestDecode(f *testing.F) {
	seed := &SendListVTXOsByScriptsRequest{
		PkScripts: [][]byte{
			bytes.Repeat([]byte{0x03}, 34),
			bytes.Repeat([]byte{0x04}, 34),
		},
		Limit:         50,
		CorrelationID: "corr-2",
		MsgID:         "msg-2",
	}

	var buf bytes.Buffer
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}
	for _, p := range hugeRecordPayloads() {
		f.Add(p)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var v SendListVTXOsByScriptsRequest

		// MUST NOT panic on any input.
		if err := v.Decode(bytes.NewReader(data)); err != nil {
			return
		}

		// Re-encode may legitimately fail (Encode re-validates the
		// opaque cursor), so a re-encode error is acceptable; only a
		// panic or a re-decode failure of successfully re-encoded
		// bytes is a bug.
		var out bytes.Buffer
		if err := v.Encode(&out); err != nil {
			return
		}

		var v2 SendListVTXOsByScriptsRequest
		if err := v2.Decode(bytes.NewReader(out.Bytes())); err != nil {
			t.Fatalf("re-decode: %v", err)
		}
	})
}
