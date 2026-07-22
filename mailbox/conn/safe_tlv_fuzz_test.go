package conn

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// FuzzAckStateDecode drives AckState.Decode with arbitrary bytes. The
// AckState blob is persisted in the durable checkpoint store and
// replayed from disk across restarts/upgrades, so a crafted record
// length must surface as an error rather than panicking the actor on
// every replay (regression for the tlv unbounded-make DoS).
func FuzzAckStateDecode(f *testing.F) {
	// Seed with a valid encoding so the fuzzer explores mutations of
	// real framing.
	var buf bytes.Buffer
	seed := &AckState{
		PullCursor:          42,
		DispatchCommittedTo: 30,
		AckTarget:           30,
		AckCommittedTo:      20,
	}
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}

	// Known regression crashers: a tiny payload that declares a record
	// length near 2^63 / 2^64 in a single type/length prefix. The
	// first hits the unknown-odd-record path (stream.go make), the
	// second the known-record path.
	f.Add([]byte{
		0x0b,
		0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})
	f.Add([]byte{
		0x03,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		var v AckState

		// MUST NOT panic on any input.
		if err := v.Decode(bytes.NewReader(data)); err != nil {
			return
		}

		// A successful decode must re-encode and re-decode cleanly so
		// the persisted form round-trips.
		var out bytes.Buffer
		if err := v.Encode(&out); err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		var v2 AckState
		if err := v2.Decode(bytes.NewReader(out.Bytes())); err != nil {
			t.Fatalf("re-decode: %v", err)
		}

		if v != v2 {
			t.Fatalf("round-trip mismatch: %+v != %+v", v, v2)
		}
	})
}

// decodeWrappedProtoStream is a test-only adapter that exercises the
// WrappedProto record decoder (which holds the make([]byte, l) proto
// allocation) through a bounded TLV reader, mirroring how serverconn
// messages decode their embedded proto payloads.
func decodeWrappedProtoStream(data []byte) error {
	rec := tlv.ZeroRecordT[
		tlv.TlvType1, WrappedProto[*wrapperspb.StringValue],
	]()
	rec.Val.Val = &wrapperspb.StringValue{}

	stream, err := tlv.NewStream(rec.Record())
	if err != nil {
		return err
	}

	safe, err := safeTLVReader(bytes.NewReader(data))
	if err != nil {
		return err
	}

	_, err = stream.DecodeWithParsedTypes(safe)

	return err
}

// FuzzWrappedProtoDecode drives the WrappedProto record decoder, which
// sizes its value buffer with make([]byte, l) from an attacker
// controlled length. WrappedProto re-encodes proto bytes (not byte
// exact for arbitrary inputs), so we assert no-panic only rather than
// round-trip equality.
func FuzzWrappedProtoDecode(f *testing.F) {
	rec := tlv.ZeroRecordT[
		tlv.TlvType1, WrappedProto[*wrapperspb.StringValue],
	]()
	rec.Val.Val = wrapperspb.String("hello")

	stream, err := tlv.NewStream(rec.Record())
	if err == nil {
		var buf bytes.Buffer
		if err := stream.Encode(&buf); err == nil {
			f.Add(buf.Bytes())
		}
	}

	// Known crasher: type 1 (the wrapped-proto field) with a huge
	// declared length in a tiny payload.
	f.Add([]byte{
		0x01,
		0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		// MUST NOT panic; an error is the acceptable outcome.
		_ = decodeWrappedProtoStream(data)
	})
}
