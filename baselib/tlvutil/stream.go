// Package tlvutil provides shared helpers that collapse the repetitive TLV
// stream scaffolding used by durable-actor message codecs across the darepo and
// darepo-client repositories.
//
// Every TLVMessage.Encode/Decode implementation would otherwise hand-write the
// same NewStream/Encode and NewStream/DecodeWithParsedTypes dance. The helpers
// here reduce a message body to a records slice plus a single call, without
// changing the wire format: they are thin wrappers over lnd/tlv that produce
// byte-identical output to the inlined form they replace.
package tlvutil

import (
	"bytes"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// EncodeRecords builds a TLV stream from the passed records and writes the
// encoded form to w. It replaces the repeated NewStream/Encode pair that every
// message Encode method would otherwise hand-write, and is byte-for-byte
// equivalent to that inlined form.
func EncodeRecords(w io.Writer, records ...tlv.Record) error {
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// EncodeRecordsToBytes encodes the passed records into a freshly allocated
// buffer and returns the resulting bytes. It is a convenience over
// EncodeRecords for codecs that need a []byte payload rather than streaming
// into an existing writer.
func EncodeRecordsToBytes(records ...tlv.Record) ([]byte, error) {
	var buf bytes.Buffer
	if err := EncodeRecords(&buf, records...); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// DecodeRecords builds a TLV stream from the passed records and decodes r into
// them, returning the parsed-type map so callers can detect which optional
// records were actually present on the wire. It replaces the repeated
// NewStream/DecodeWithParsedTypes pair found in every message Decode method.
func DecodeRecords(r io.Reader, records ...tlv.Record) (tlv.TypeMap, error) {
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	return stream.DecodeWithParsedTypes(r)
}

// DecodeRecordsFromBytes decodes the passed byte slice into the given records,
// returning the parsed-type map. It is a convenience over DecodeRecords for
// codecs that hold a []byte payload rather than a reader.
func DecodeRecordsFromBytes(b []byte,
	records ...tlv.Record) (tlv.TypeMap, error) {

	return DecodeRecords(bytes.NewReader(b), records...)
}
