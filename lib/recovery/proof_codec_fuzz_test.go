package recovery

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
)

// FuzzDecodeProof drives attacker-controlled bytes through the recovery proof
// decoder. Proof bytes are persisted durably and rebuilt on restart, so the
// decoder must never panic or allocate an unbounded buffer from a declared TLV
// length. The vulnerable surfaces are the outer DVarBytes records
// (targetOutpoint, nodes), the nested per-Node sub-stream decoder's
// make([]byte, l), and the wire.MsgTx deserializer inside each node.
//
// DecodeProof routes through NewProof, which enforces structural invariants
// (cycle-freedom, reachability, caps). A successful decode therefore yields a
// canonical proof that must re-encode and re-decode cleanly.
func FuzzDecodeProof(f *testing.F) {
	if b := fuzzSeedProof(); b != nil {
		f.Add(b)
	}

	f.Add([]byte{})
	f.Add([]byte{0x00})

	// targetOutpoint is TLV type 3 and decodes via DVarBytes. A tiny
	// payload declaring a huge length is the canonical unbounded-make
	// crasher.
	f.Add([]byte{
		0x03, 0xfe, 0xff, 0xff, 0xff, 0xff,
	})

	// The nodes record is TLV type 7, also DVarBytes-backed.
	f.Add([]byte{
		0x07, 0xfe, 0xff, 0xff, 0xff, 0xff,
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
		got, err := DecodeProof(data)
		if err != nil {
			return
		}

		// A canonical proof must round-trip through encode/decode.
		b2, err := EncodeProof(got)
		if err != nil {
			t.Fatalf("re-encode decoded proof: %v", err)
		}

		if _, err := DecodeProof(b2); err != nil {
			t.Fatalf("re-decode re-encoded proof: %v", err)
		}
	})
}

// fuzzSeedProof builds a minimal valid proof and returns its encoded bytes to
// seed the corpus. Errors yield a nil return so the seed is skipped rather than
// failing target setup.
func fuzzSeedProof() []byte {
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{})
	tx.AddTxOut(&wire.TxOut{Value: 1000})

	p, err := NewProof(
		wire.OutPoint{Hash: tx.TxHash()},
		10,
		&Node{Kind: NodeKindArk, Tx: tx},
	)
	if err != nil {
		return nil
	}

	b, err := EncodeProof(p)
	if err != nil {
		return nil
	}

	return b
}
