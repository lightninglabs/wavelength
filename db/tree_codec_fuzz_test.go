package db

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
)

// FuzzDeserializeTree drives DeserializeTree with arbitrary bytes. Tree
// blobs are fed from the durable mailbox, persisted rows, and
// operator-supplied indexer responses, all untrusted, so a crafted
// blob must surface as an error rather than panicking (regressions for
// the tlv unbounded-make DoS: DVarBytes SweepRoot/RootNode lengths,
// txOutsDecoder numOutputs, pubKeysDecoder numKeys, and per-output
// script lengths). DeserializeTree re-derives a tree.Tree but the
// serialized form is not guaranteed byte-stable across the map-keyed
// children, so a successful decode asserts re-serialize succeeds and
// re-deserializes cleanly rather than byte equality.
func FuzzDeserializeTree(f *testing.F) {
	// Seed with a valid minimal tree so the fuzzer mutates real
	// framing.
	minimal := &tree.Tree{
		BatchOutpoint: wire.OutPoint{},
		Root: &tree.Node{
			Input:     wire.OutPoint{},
			Outputs:   []*wire.TxOut{},
			CoSigners: []*btcec.PublicKey{},
			Children:  make(map[uint32]*tree.Node),
		},
	}
	if data, err := SerializeTree(minimal); err == nil {
		f.Add(data)
	}

	// Seed with a tree carrying an output and a cosigner key so the
	// element-count decoders are reachable from the corpus.
	withData := &tree.Tree{
		BatchOutpoint: wire.OutPoint{Hash: chainhash.Hash{0x01}},
		Root: &tree.Node{
			Input: wire.OutPoint{Hash: chainhash.Hash{0x02}},
			Outputs: []*wire.TxOut{
				{Value: 1000, PkScript: bytes.Repeat(
					[]byte{0x51}, 34,
				)},
			},
			CoSigners: []*btcec.PublicKey{},
			Children:  make(map[uint32]*tree.Node),
		},
	}
	if data, err := SerializeTree(withData); err == nil {
		f.Add(data)
	}

	// Known regression crashers reaching the outer DVarBytes and the
	// inner element-count makes.
	f.Add([]byte{
		// SweepRoot (type 2) with a near-2^63 declared length.
		0x02, 0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})
	f.Add([]byte{
		// RootNodeData (type 3) holding a node blob whose outputs
		// record (type 1) declares a huge numOutputs.
		0x03, 0x0b,
		0x01, 0x09,
		0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		// MUST NOT panic on any input.
		tr, err := DeserializeTree(data)
		if err != nil {
			return
		}

		// A successfully decoded tree must re-serialize and
		// re-deserialize cleanly.
		out, err := SerializeTree(tr)
		if err != nil {
			t.Fatalf("re-serialize: %v", err)
		}

		if _, err := DeserializeTree(out); err != nil {
			t.Fatalf("re-deserialize: %v", err)
		}
	})
}
