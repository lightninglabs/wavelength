package actor

import (
	"bytes"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// These fuzz tests target the actor-framework TLV decoders, which run on
// attacker-controlled bytes replayed from durable mailboxes across rolling
// upgrades. The contract is uniform: a decoder MUST return an error (never
// panic, OOM, or slice-OOB) on malformed input, and any value it accepts
// MUST round-trip back through its encoder.

// FuzzAskResponseDecode exercises the durable AskResponse decoder. Its
// ResultBlob is a []byte record sized from a declared length, the unbounded
// make() sink the framing pre-validation now bounds.
func FuzzAskResponseDecode(f *testing.F) {
	var buf bytes.Buffer
	seed := AskResponse{
		CorrelationID: "corr-1",
		ResultBlob:    []byte{1, 2, 3, 4},
		ErrorText:     "boom",
	}
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}
	f.Add([]byte{})

	// A []byte record (type 2) declaring a near-2^64 length in a tiny
	// envelope: the historical makeslice crasher.
	f.Add([]byte{
		0x02, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		var v AskResponse
		if err := v.Decode(bytes.NewReader(data)); err != nil {
			return
		}

		var out bytes.Buffer
		if err := v.Encode(&out); err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		var v2 AskResponse
		if err := v2.Decode(bytes.NewReader(out.Bytes())); err != nil {
			t.Fatalf("re-decode: %v", err)
		}
	})
}

// FuzzRestartMessageDecode exercises the RestartMessage checkpoint decoder.
// Its actorID/stateType/stateData are []byte records sized from declared
// lengths.
func FuzzRestartMessageDecode(f *testing.F) {
	var buf bytes.Buffer
	seed := &RestartMessage{
		Checkpoint: fn.Some(Checkpoint{
			ActorID:   "actor-1",
			StateType: "state",
			StateData: []byte{9, 8, 7},
			Version:   3,
			UpdatedAt: time.Unix(1000, 0),
		}),
	}
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}
	f.Add([]byte{})
	f.Add([]byte{
		0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		var v RestartMessage
		if err := v.Decode(bytes.NewReader(data)); err != nil {
			return
		}

		var out bytes.Buffer
		if err := v.Encode(&out); err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		var v2 RestartMessage
		if err := v2.Decode(bytes.NewReader(out.Bytes())); err != nil {
			t.Fatalf("re-decode: %v", err)
		}
	})
}

// FuzzMessageCodecDecode exercises the top-level envelope decoder that every
// durable mailbox message flows through. Its payload is read into
// make([]byte, payloadLen) sized from a declared length, the most reachable
// unbounded-alloc sink in the actor framework.
func FuzzMessageCodecDecode(f *testing.F) {
	codec := NewMessageCodec()
	codec.MustRegister(AskResponseMsgType, func() TLVMessage {
		return &AskResponse{}
	})

	resp := &AskResponse{
		CorrelationID: "c",
		ResultBlob:    []byte{1, 2},
	}
	if raw, err := codec.Encode(resp); err == nil {
		f.Add(raw)
	}
	f.Add([]byte{})

	// type=AskResponseMsgType varint, then a near-2^64 payload length in a
	// tiny envelope: the historical makeslice crasher at the codec layer.
	f.Add([]byte{
		0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		// The codec round-trip is not byte-exact across all registered
		// types, so assert only the no-panic decode contract here.
		_, _ = codec.Decode(data)
	})
}
