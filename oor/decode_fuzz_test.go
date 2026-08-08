package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/tlv"
)

// These fuzz tests target the OOR durable-message decoders, which run on
// attacker-controlled bytes that cross the client/server trust boundary and
// persist in durable actor mailboxes replayed across upgrades. The
// invariant under test is uniform: a decoder MUST return an error (never
// panic, OOM, or slice-OOB) on malformed input, and any value it accepts
// MUST round-trip back through its encoder.

// seedBlobList encodes a small valid length-prefixed blob list so the fuzz
// corpus starts from a structurally valid seed.
func seedBlobList(tb testing.TB) []byte {
	tb.Helper()

	raw, err := encodeLengthPrefixedBlobList([][]byte{
		{1, 2, 3}, {}, {4, 5},
	})
	if err != nil {
		tb.Fatalf("encode blob list: %v", err)
	}

	return raw
}

// FuzzDecodeLengthPrefixedBlobList exercises the shared length-prefixed blob
// list decoder, the helper every nested OOR list decoder funnels through.
// Its per-element make([]byte, elementLen) is the primary unbounded-alloc
// sink reachable from a crafted count/length prefix.
func FuzzDecodeLengthPrefixedBlobList(f *testing.F) {
	f.Add(seedBlobList(f))
	f.Add([]byte{})
	f.Add([]byte{0x00})

	// A count of 1 followed by a huge declared element length in a tiny
	// payload: the historical crasher for the unbounded make([]byte, ...).
	f.Add([]byte{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		blobs, err := decodeLengthPrefixedBlobList(data)
		if err != nil {
			return
		}

		// A value that decoded must re-encode and re-decode cleanly.
		out, err := encodeLengthPrefixedBlobList(blobs)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		if _, err := decodeLengthPrefixedBlobList(out); err != nil {
			t.Fatalf("re-decode: %v", err)
		}
	})
}

// FuzzDecodeOutgoingSnapshot exercises the outgoing-snapshot TLV decoder,
// the top-level durable state blob for an outgoing OOR transfer.
func FuzzDecodeOutgoingSnapshot(f *testing.F) {
	snap := &OutgoingSnapshot{
		Version:         4,
		SessionID:       SessionID(chainhash.Hash{1, 2, 3}),
		Phase:           OutgoingPhaseSubmitSent,
		ArkPSBT:         []byte{1, 2, 3, 4},
		CheckpointPSBTs: [][]byte{{5, 6}, {7, 8}},
		FailReason:      "boom",
		IdempotencyKey:  "key",
	}
	if raw, err := encodeOutgoingSnapshot(snap); err == nil {
		f.Add(raw)
	}
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := decodeOutgoingSnapshot(data)
		if err != nil {
			return
		}

		out, err := encodeOutgoingSnapshot(got)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		if _, err := decodeOutgoingSnapshot(out); err != nil {
			t.Fatalf("re-decode: %v", err)
		}
	})
}

// FuzzDecodeIncomingSnapshot exercises the incoming-snapshot TLV decoder,
// the top-level durable state blob for an incoming OOR transfer (carries
// ancestor packages and checkpoint lists).
func FuzzDecodeIncomingSnapshot(f *testing.F) {
	snap := &IncomingSnapshot{
		Version:           1,
		SessionID:         SessionID(chainhash.Hash{9, 8, 7}),
		Phase:             IncomingPhaseResolvePending,
		ArkPSBT:           []byte{1, 2},
		CheckpointPSBTs:   [][]byte{{3, 4}},
		FailReason:        "nope",
		RecipientPkScript: []byte{0xaa, 0xbb},
		RecipientEventID:  42,
		MetadataAttempts:  2,
		ResolveAttempts:   1,
	}
	if raw, err := encodeIncomingSnapshot(snap); err == nil {
		f.Add(raw)
	}
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := decodeIncomingSnapshotWithLimits(
			data, DefaultReceiveLimits(),
		)
		if err != nil {
			return
		}

		// AncestorPackages contains *psbt.Packet values whose re-encode
		// is not guaranteed byte-exact, so assert no-panic re-encode
		// rather than byte equality.
		if _, err := encodeIncomingSnapshot(got); err != nil {
			t.Fatalf("re-encode: %v", err)
		}
	})
}

// FuzzDecodeStartTransferPayload exercises the start-transfer payload
// decoder, which carries the operator key, transfer inputs, and recipients
// of an outgoing OOR send request.
func FuzzDecodeStartTransferPayload(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x01, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		// Decode under bounded limits; the contract is no-panic.
		_, _ = decodeStartTransferPayloadWithLimits(
			data, DefaultReceiveLimits(),
		)
	})
}

// FuzzDecodePackageArtifact exercises the single package-artifact TLV
// decoder, which carries a session id, an Ark PSBT, and a checkpoint list.
func FuzzDecodePackageArtifact(f *testing.F) {
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = decodePackageArtifactWithLimits(
			data, DefaultReceiveLimits(),
		)
	})
}

// FuzzDecodeConditionWitness exercises the hand-rolled condition-witness
// list decoder (wire.ReadVarInt count + wire.ReadVarBytes items).
func FuzzDecodeConditionWitness(f *testing.F) {
	if raw, err := encodeConditionWitness(
		[][]byte{{1, 2}, {3}},
	); err == nil {
		f.Add(raw)
	}
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = decodeConditionWitness(data)
	})
}

// FuzzDecodeExternalSignatures exercises the external-signature list decoder
// (count varint plus three var-byte fields and a fixed sighash per item).
func FuzzDecodeExternalSignatures(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = decodeExternalSignatures(data)
	})
}

// FuzzDecodeOutPointList exercises the outpoint-list decoder, which sizes a
// wire.OutPoint slice from a count prefix.
func FuzzDecodeOutPointList(f *testing.F) {
	if raw, err := encodeOutPointList([]wire.OutPoint{
		{Hash: chainhash.Hash{1}, Index: 7},
	}); err == nil {
		f.Add(raw)
	}
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = decodeOutPointListWithLimits(data, DefaultReceiveLimits())
	})
}

// FuzzDecodeRestoreSnapshotPayload exercises the restore-payload decoder
// which wraps an embedded outgoing snapshot in an outer TLV record.
func FuzzDecodeRestoreSnapshotPayload(f *testing.F) {
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = decodeRestoreSnapshotPayloadWithLimits(
			data, DefaultReceiveLimits(),
		)
	})
}

// FuzzDecodeEventPayload exercises the FSM event-payload decoder, the body
// of every durable DriveEventRequest. Its event-kind switch fans out into
// PSBT lists, ancestor packages, metadata matches, and outpoint lists, so it
// is the single richest attacker-controlled decode surface in the package and
// is not otherwise reached by the top-level snapshot decoders. The contract
// is no-panic: most event kinds carry *psbt.Packet values whose re-encode is
// not byte-exact, so a round-trip assertion is not meaningful here.
func FuzzDecodeEventPayload(f *testing.F) {
	// A FailEvent has no PSBT payload, so it both encodes and decodes
	// cleanly and gives the fuzzer a structurally valid seed for the
	// event-kind switch.
	if raw, err := encodeEventPayload(
		&FailEvent{Reason: "boom"},
	); err == nil {
		f.Add(raw)
	}
	f.Add([]byte{})
	f.Add([]byte{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = decodeEventPayloadWithLimits(data, DefaultReceiveLimits())
	})
}

// FuzzDecodeDriveEventRequestPayload exercises the outer DriveEventRequest
// payload decoder (session id + embedded event payload). DriveEventRequest is
// a TLV-durable message replayed from the mailbox, so its framing crosses the
// trust boundary independently of the inner event body fuzzed above.
func FuzzDecodeDriveEventRequestPayload(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = decodeDriveEventRequestPayloadWithLimits(
			data, DefaultReceiveLimits(),
		)
	})
}

// FuzzDecodeRecipientPayload exercises the single-recipient TLV decoder, which
// narrows an attacker-supplied uint64 value into int64 and carries two
// variable-length byte fields. It is reached only nested under start-transfer
// today; a direct target lets the fuzzer explore its value-overflow guard and
// byte-field framing without first constructing a valid outer list.
func FuzzDecodeRecipientPayload(f *testing.F) {
	if raw, err := encodeRecipientPayload(recipientPayload{
		PkScript:           []byte{0xaa, 0xbb},
		ValueSat:           1234,
		VTXOPolicyTemplate: []byte{0x01},
	}); err == nil {
		f.Add(raw)
	}
	f.Add([]byte{})
	f.Add([]byte{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := decodeRecipientPayload(data)
		if err != nil {
			return
		}

		out, err := encodeRecipientPayload(got)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		if _, err := decodeRecipientPayload(out); err != nil {
			t.Fatalf("re-decode: %v", err)
		}
	})
}

// FuzzDecodeUint32List exercises the hand-rolled big-endian uint32 list
// decoder used inside ancestry entries. It sizes make([]uint32, count) from a
// 4-byte count prefix and is the package's only length-prefixed list decoder
// that bypasses checkElemCount, relying instead on an explicit
// implied-length equality check; a direct target stresses that bound against
// crafted count/length mismatches that could otherwise wrap on 32-bit
// platforms.
func FuzzDecodeUint32List(f *testing.F) {
	f.Add(encodeUint32List([]uint32{1, 2, 3}))
	f.Add([]byte{})

	// A count of 0xffffffff with no backing bytes: the over-allocation /
	// int-wrap probe for the make([]uint32, count) sink.
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := decodeUint32List(data)
		if err != nil {
			return
		}

		// A decoded list must re-encode and re-decode byte-stably.
		if _, err := decodeUint32List(encodeUint32List(got)); err != nil {
			t.Fatalf("re-decode: %v", err)
		}
	})
}

// FuzzValidateTLVRecordLengths exercises the framing pre-validator directly
// so a malformed varint or over-declared record length is rejected without
// panicking before the real decoder ever allocates.
func FuzzValidateTLVRecordLengths(f *testing.F) {
	var buf bytes.Buffer
	val := []byte{1, 2, 3}
	rec := tlv.MakePrimitiveRecord(1, &val)
	if stream, err := tlv.NewStream(rec); err == nil {
		if err := stream.Encode(&buf); err == nil {
			f.Add(buf.Bytes())
		}
	}
	f.Add([]byte{})
	f.Add([]byte{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		// Pure no-panic contract; either outcome is acceptable.
		_ = validateTLVRecordLengths(data)
	})
}
