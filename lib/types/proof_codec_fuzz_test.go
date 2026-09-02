package types

import (
	"testing"
)

// FuzzDeserializeTxProof drives attacker-controlled bytes through the TxProof
// TLV decoder. TxProof bytes cross the trust boundary as a boarding SPV proof
// the server verifies and also persist durably, so the decoder must never
// panic or allocate an unbounded buffer from a declared TLV length (notably
// the variable-length MerkleRoot record and the unknown-record discard path).
//
// The encode path is not byte-for-byte symmetric for every decodable value
// (a decoded proof may omit optional records the encoder always writes), so we
// assert the no-panic property and that any successful decode can be
// re-serialized without error rather than a strict byte round-trip.
func FuzzDeserializeTxProof(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})

	// MerkleRoot is TLV type 6 and decodes via DVarBytes. A tiny payload
	// declaring a huge length is the canonical unbounded-make crasher.
	f.Add([]byte{
		0x06, 0xfe, 0xff, 0xff, 0xff, 0xff,
	})

	// The MsgTx record is type 0 and feeds wire.MsgTx.Deserialize, which
	// reads its own attacker-controlled var-length prefixes. Probe it with
	// a declared-large outer length and an empty body.
	f.Add([]byte{
		0x00, 0xfe, 0xff, 0xff, 0xff, 0xff,
	})

	// The canonical near-int64-max length crasher: type 0x0b followed by an
	// 8-byte BigSize (0xff prefix) declaring 0x7fffffffffffffff value bytes
	// in a 10-byte envelope. On the unhardened path this drives
	// make([]byte, declaredLength) to "makeslice: cap out of range"; the
	// framing pre-validator must reject it before any allocation. Pinned as
	// a deterministic regression seed so the guard runs on every test pass.
	f.Add([]byte{
		0x0b, 0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		// The primary invariant: decoding hostile bytes must return an
		// error, never panic or exhaust memory.
		got, err := DeserializeTxProof(data)
		if err != nil {
			return
		}

		// A nil proof (empty input) is a valid no-op decode result.
		if got == nil {
			return
		}

		// Anything that decoded must re-serialize without error. We do
		// not assert byte equality because optional records can differ.
		if _, err := SerializeTxProof(got); err != nil {
			t.Fatalf("re-serialize decoded proof: %v", err)
		}
	})
}
