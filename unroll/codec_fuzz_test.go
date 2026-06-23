package unroll

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/unrollplan"
)

// canonicalMaliciousTLV is the standard over-allocation probe: TLV type 11,
// then a BigSize-encoded length of math.MaxInt64. Without the safeTLVReader
// framing guard the decoder would size make([]byte, declaredLength) from this
// value and panic (makeslice) or OOM. Seeding it into every TLV-framed target
// pins the guard's rejection path into the persistent corpus.
var canonicalMaliciousTLV = []byte{
	0x0b, 0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
}

// fuzzDecodeMsg is a small generic helper that exercises one durable mailbox
// message decoder against attacker-controlled bytes and, on a clean decode,
// re-encodes/re-decodes to confirm the value is self-consistent. The durable
// mailbox replays these payloads from disk across rolling upgrades, so a
// decode must never panic.
func fuzzDecodeMsg[T Msg](t *testing.T, data []byte, mk func() T) {
	v := mk()
	if err := v.Decode(bytes.NewReader(data)); err != nil {
		return
	}

	var out bytes.Buffer
	if err := v.Encode(&out); err != nil {
		t.Fatalf("re-encode: %v", err)
	}

	v2 := mk()
	if err := v2.Decode(bytes.NewReader(out.Bytes())); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
}

// FuzzStartUnrollRequestDecode fuzzes the StartUnrollRequest mailbox decoder.
// Its ExitPolicyKind/ExitPolicyRef records decode through tlv DVarBytes, the
// unbounded-allocation path.
func FuzzStartUnrollRequestDecode(f *testing.F) {
	seed := &StartUnrollRequest{
		Height:         42,
		Trigger:        TriggerFraudSpend,
		ExitPolicyKind: ExitPolicyKind("custom"),
		ExitPolicyRef:  "ref-123",
	}
	var buf bytes.Buffer
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}
	f.Add([]byte{})
	f.Add(canonicalMaliciousTLV)

	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzDecodeMsg(t, data, func() *StartUnrollRequest {
			return &StartUnrollRequest{}
		})
	})
}

// FuzzTxFailedMsgDecode fuzzes TxFailedMsg, whose Reason record decodes through
// the unbounded DVarBytes path.
func FuzzTxFailedMsgDecode(f *testing.F) {
	seed := &TxFailedMsg{
		Txid:   chainhash.Hash{0x01, 0x02},
		Reason: "broadcast rejected",
	}
	var buf bytes.Buffer
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}
	f.Add([]byte{})
	f.Add(canonicalMaliciousTLV)

	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzDecodeMsg(t, data, func() *TxFailedMsg {
			return &TxFailedMsg{}
		})
	})
}

// FuzzSpendObservedMsgDecode fuzzes SpendObservedMsg, which carries multiple
// fixed-size records that must tolerate truncation and length lies.
func FuzzSpendObservedMsgDecode(f *testing.F) {
	seed := &SpendObservedMsg{
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{0xaa},
			Index: 7,
		},
		SpendingTxid:   chainhash.Hash{0xbb},
		SpendingHeight: 100,
	}
	var buf bytes.Buffer
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}
	f.Add([]byte{})
	f.Add(canonicalMaliciousTLV)

	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzDecodeMsg(t, data, func() *SpendObservedMsg {
			return &SpendObservedMsg{}
		})
	})
}

// FuzzResumeUnrollRequestDecode fuzzes the ResumeUnrollRequest mailbox decoder.
// It carries a single fixed-size height record but still routes through
// safeDecodeStream, so a length lie in the framing must fail closed.
func FuzzResumeUnrollRequestDecode(f *testing.F) {
	seed := &ResumeUnrollRequest{Height: 99}
	var buf bytes.Buffer
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}
	f.Add([]byte{})
	f.Add(canonicalMaliciousTLV)

	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzDecodeMsg(t, data, func() *ResumeUnrollRequest {
			return &ResumeUnrollRequest{}
		})
	})
}

// FuzzHeightObservedMsgDecode fuzzes the HeightObservedMsg mailbox decoder.
func FuzzHeightObservedMsgDecode(f *testing.F) {
	seed := &HeightObservedMsg{Height: 12345}
	var buf bytes.Buffer
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}
	f.Add([]byte{})
	f.Add(canonicalMaliciousTLV)

	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzDecodeMsg(t, data, func() *HeightObservedMsg {
			return &HeightObservedMsg{}
		})
	})
}

// FuzzTxConfirmedMsgDecode fuzzes the TxConfirmedMsg mailbox decoder. Its
// fixed-size txid record must tolerate truncation and declared-length lies.
func FuzzTxConfirmedMsgDecode(f *testing.F) {
	seed := &TxConfirmedMsg{
		Txid:     chainhash.Hash{0xab, 0xcd},
		Height:   77,
		NumConfs: 6,
	}
	var buf bytes.Buffer
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}
	f.Add([]byte{})
	f.Add(canonicalMaliciousTLV)

	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzDecodeMsg(t, data, func() *TxConfirmedMsg {
			return &TxConfirmedMsg{}
		})
	})
}

// FuzzDecodeCheckpoint fuzzes the full actor checkpoint codec. The checkpoint
// embeds the planner state, an optional serialized sweep tx, the deferred
// checkpoint list, and several DVarBytes records — every unbounded-allocation
// vector in the package converges here.
func FuzzDecodeCheckpoint(f *testing.F) {
	state := unrollplan.State{
		ConfirmedTxids: []chainhash.Hash{{0x01}},
		InFlightTxids:  []chainhash.Hash{{0x02}},
	}
	cp := &actorCheckpoint{
		Version:        checkpointVersion,
		Height:         123,
		Started:        true,
		Trigger:        TriggerManual,
		State:          state,
		ExitPolicyKind: ExitPolicyKind("custom"),
		ExitPolicyRef:  "ref",
		Fail:           "boom",
		SweepAttempts:  2,
		DeferredCheckpoints: []DeferredCheckpoint{
			{Txid: chainhash.Hash{0x03}, DeadlineHeight: 9},
		},
	}
	if seed, err := encodeCheckpoint(cp); err == nil {
		f.Add(seed)
	}
	f.Add([]byte{})
	f.Add(canonicalMaliciousTLV)

	f.Fuzz(func(t *testing.T, data []byte) {
		decoded, err := decodeCheckpoint(data)
		if err != nil {
			return
		}

		out, err := encodeCheckpoint(decoded)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		if _, err := decodeCheckpoint(out); err != nil {
			t.Fatalf("re-decode: %v", err)
		}
	})
}

// FuzzDecodeDeferredCheckpoints targets the deferred-checkpoint list codec
// directly. Its leading varint count drives make([]DeferredCheckpoint, 0,
// count) and each per-entry varint length drives make([]byte, entryLen) — both
// sized from attacker input.
func FuzzDecodeDeferredCheckpoints(f *testing.F) {
	if seed, err := encodeDeferredCheckpoints([]DeferredCheckpoint{
		{Txid: chainhash.Hash{0x01}, DeadlineHeight: 1},
		{Txid: chainhash.Hash{0x02}, DeadlineHeight: 2},
	}); err == nil {
		f.Add(seed)
	}
	f.Add([]byte{})

	// Canonical malicious payload: a BigSize-encoded count of math.MaxInt64.
	// decodeDeferredCheckpoints reads this count first and must reject it
	// against the remaining-bytes bound before make([]DeferredCheckpoint, 0,
	// count) can over-allocate.
	f.Add([]byte{0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		decoded, err := decodeDeferredCheckpoints(data)
		if err != nil {
			return
		}

		// The per-entry codec is deliberately lenient (it skips unknown
		// odd TLV types for forward-compat), so a re-encode is not
		// byte-exact against an adversarial input. We instead assert a
		// stable fixed point: encode the decoded value and confirm it
		// re-decodes to an equal value. The first encode IS canonical,
		// so the second round must be byte-stable.
		out, err := encodeDeferredCheckpoints(decoded)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		redecoded, err := decodeDeferredCheckpoints(out)
		if err != nil {
			t.Fatalf("re-decode: %v", err)
		}

		reencoded, err := encodeDeferredCheckpoints(redecoded)
		if err != nil {
			t.Fatalf("re-encode 2: %v", err)
		}

		if !bytes.Equal(out, reencoded) {
			t.Fatalf("deferred checkpoints not a stable fixed "+
				"point:\n got %x\nwant %x", reencoded, out)
		}
	})
}
