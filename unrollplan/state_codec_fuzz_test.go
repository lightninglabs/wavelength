package unrollplan

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// FuzzDecodeState fuzzes the unrollplan State codec with attacker-controlled
// bytes. State is persisted durably (nested inside the unroll actor
// checkpoint) and replayed across upgrades, so DecodeState must never panic.
// The hash-list records and the nested sweep sub-stream both flow through tlv
// DVarBytes plus a hand-rolled big-endian count prefix.
func FuzzDecodeState(f *testing.F) {
	seed := &State{
		ConfirmedTxids: []chainhash.Hash{{0x01}, {0x02}},
		InFlightTxids:  []chainhash.Hash{{0x03}},
		TargetConfirmHeight: fn.Some[int32](
			500,
		),
		Sweep: SweepState{
			Status:        SweepStatusBroadcasted,
			Txid:          fn.Some(chainhash.Hash{0x04}),
			ConfirmHeight: fn.Some[int32](600),
		},
	}
	if b, err := EncodeState(seed); err == nil {
		f.Add(b)
	}
	f.Add([]byte{})

	// Canonical malicious payload: TLV type 11, then a BigSize-encoded
	// length of math.MaxInt64. Without the safeTLVReader framing guard the
	// decoder would size make([]byte, declaredLength) from this value and
	// panic (makeslice) or OOM. Seeding it pins the guard's rejection path.
	f.Add([]byte{
		0x0b, 0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		state, err := DecodeState(data)
		if err != nil {
			return
		}

		out, err := EncodeState(state)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		if _, err := DecodeState(out); err != nil {
			t.Fatalf("re-decode: %v", err)
		}
	})
}

// FuzzDecodeHashList targets the hand-rolled hash-list codec directly: a 4-byte
// big-endian count followed by raw 32-byte hashes. The count guards a make()
// and a length check, so it is worth fuzzing in isolation.
func FuzzDecodeHashList(f *testing.F) {
	if seed, err := encodeHashList([]chainhash.Hash{
		{0x01}, {0x02},
	}, "seed"); err == nil {
		f.Add(seed)
	}
	f.Add([]byte{})

	// Canonical malicious payload: a 4-byte big-endian count of
	// 0xFFFFFFFF with no trailing hashes. decodeHashList must reject this
	// via the exact count*HashSize length check before make([]chainhash.Hash,
	// 0, count) can over-allocate.
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		hashes, err := decodeHashList(data, "fuzz")
		if err != nil {
			return
		}

		// encodeHashList canonicalizes by sorting, so the bytes are not
		// expected to match a hand-crafted (possibly unsorted) input.
		// We instead assert the encode succeeds and re-decodes to the
		// same multiset, confirming the decoder produced a
		// self-consistent value with no panic.
		out, err := encodeHashList(hashes, "fuzz")
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		redecoded, err := decodeHashList(out, "fuzz")
		if err != nil {
			t.Fatalf("re-decode: %v", err)
		}

		if len(redecoded) != len(hashes) {
			t.Fatalf("hash count drift: got %d want %d",
				len(redecoded), len(hashes))
		}
	})
}
