package actortest

import (
	"bytes"
	"testing"
)

// FuzzForwardMsgDecode exercises the ForwardMsg decoder. Its Target and
// Payload are []byte records sized from declared lengths, the unbounded
// make() sink the framing pre-validation now bounds. ForwardMsg flows
// through the durable outbox/mailbox path in the actor integration tests.
func FuzzForwardMsgDecode(f *testing.F) {
	var buf bytes.Buffer
	seed := &ForwardMsg{
		Target:  "actor-b",
		MsgType: IncrementMsgType,
		Payload: []byte{1, 2, 3},
	}
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}
	f.Add([]byte{})

	// A []byte record (type 4 payload) declaring a near-2^64 length in a
	// tiny envelope: the historical makeslice crasher.
	f.Add([]byte{
		0x04, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		var v ForwardMsg
		if err := v.Decode(bytes.NewReader(data)); err != nil {
			return
		}

		var out bytes.Buffer
		if err := v.Encode(&out); err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		var v2 ForwardMsg
		if err := v2.Decode(bytes.NewReader(out.Bytes())); err != nil {
			t.Fatalf("re-decode: %v", err)
		}
	})
}

// FuzzIncrementMsgDecode exercises the IncrementMsg decoder. It carries a
// single uint64 scalar record; the test asserts the no-panic decode
// contract on arbitrary bytes.
func FuzzIncrementMsgDecode(f *testing.F) {
	var buf bytes.Buffer
	seed := &IncrementMsg{Amount: 42}
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		var v IncrementMsg
		if err := v.Decode(bytes.NewReader(data)); err != nil {
			return
		}

		var out bytes.Buffer
		if err := v.Encode(&out); err != nil {
			t.Fatalf("re-encode: %v", err)
		}
	})
}
