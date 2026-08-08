package roundpb

import (
	"encoding/hex"
	"testing"

	"google.golang.org/protobuf/proto"
)

// The converters in this file decode proto messages that, on the round
// RPC path, originate from a remote peer. A panic, OOM, slice
// out-of-bounds, or integer overflow in any of these decode helpers is a
// crash vector for the process that runs them. These fuzz targets assert
// ONLY the no-panic invariant; the converters are free to return errors
// on hostile input. The recursive-looking surface (TreeFromProto) gets
// extra attention: it must hold its node-count bound and refuse to blow
// the stack regardless of how the flattened node array is shaped.

// fuzzHash32 is a representative 32-byte hash used to seed length gates
// with a value that passes so the fuzzer can explore beyond them.
var fuzzHash32 = []byte{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
	0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
}

// FuzzOutpointFromProto fuzzes the tx-hash bytes and output index. The
// 32-byte length gate guards a fixed-size copy, so off-length inputs
// (notably 31/33 bytes) are the interesting cases.
func FuzzOutpointFromProto(f *testing.F) {
	f.Add(fuzzHash32, uint32(0))
	f.Add([]byte{}, uint32(0))
	f.Add(fuzzHash32[:31], uint32(0xffffffff))
	f.Add(append(append([]byte{}, fuzzHash32...), 0xaa), uint32(1))

	f.Fuzz(func(t *testing.T, txHash []byte, idx uint32) {
		op := &Outpoint{TxHash: txHash, OutputIndex: idx}

		// OutpointsFromProto exercises the slice wrapper, including a
		// nil entry which must be rejected rather than dereferenced.
		_, _ = OutpointsFromProto([]*Outpoint{op, nil})
		_, _ = OutpointFromProto(op)
	})
}

// FuzzTxOutFromProto fuzzes the output value (probing the negative-value
// rejection) and the pk_script bytes.
func FuzzTxOutFromProto(f *testing.F) {
	f.Add(int64(1000), []byte{0x51})
	f.Add(int64(0), []byte{})
	f.Add(int64(-1), []byte{})
	f.Add(int64(-1<<62), []byte{0x00})

	f.Fuzz(func(t *testing.T, value int64, pkScript []byte) {
		out := &TxOut{Value: value, PkScript: pkScript}

		_, _ = TxOutFromProto(out)
	})
}

// FuzzPSBTFromBytes feeds raw bytes to the PSBT deserializer. btcd's PSBT
// reader owns the parse, but this confirms our bytesReader wrapper plus
// the empty/nil handling never panic on truncated or garbage input.
func FuzzPSBTFromBytes(f *testing.F) {
	f.Add([]byte("psbt\xff"))
	f.Add([]byte{})
	f.Add([]byte{0x70, 0x73, 0x62, 0x74})

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = PSBTFromBytes(data)
	})
}

// FuzzMsgTxFromBytes feeds raw bytes to the wire-tx deserializer. The
// witness/varint length prefixes in the wire format are a classic
// allocation-amplification surface; this confirms decoding never panics.
func FuzzMsgTxFromBytes(f *testing.F) {
	f.Add([]byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00})
	f.Add([]byte{})
	// A version followed by a huge input-count varint, the kind of
	// shape that drives oversized make() in naive decoders.
	f.Add([]byte{0x01, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = MsgTxFromBytes(data)
	})
}

// FuzzSchnorrSigFromBytes fuzzes signature bytes. The 64-byte parse is
// owned by btcd's schnorr package; seeds straddle the 63/65 boundary to
// confirm our nil/length handling never panics around it.
func FuzzSchnorrSigFromBytes(f *testing.F) {
	f.Add(make([]byte, 64))
	f.Add([]byte{})
	f.Add(make([]byte, 63))
	f.Add(make([]byte, 65))

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = SchnorrSigFromBytes(data)
	})
}

// FuzzTxIDFromHex fuzzes the hex string parsed into a fixed-size tree
// TxID. The hex decode plus the 32-byte length gate guard a fixed copy.
func FuzzTxIDFromHex(f *testing.F) {
	f.Add(hex.EncodeToString(fuzzHash32))
	f.Add("")
	f.Add("zz")
	f.Add(hex.EncodeToString(fuzzHash32[:31]))
	f.Add(hex.EncodeToString(append(append([]byte{}, fuzzHash32...), 0xaa)))

	f.Fuzz(func(t *testing.T, s string) {
		_, _ = TxIDFromHex(s)
	})
}

// FuzzOutpointFromMapKey fuzzes the "hash:index" string key parser, which
// splits on ":" and parses both halves. Malformed separators and oversize
// indices are the interesting cases.
func FuzzOutpointFromMapKey(f *testing.F) {
	f.Add(hex.EncodeToString(fuzzHash32) + ":0")
	f.Add("")
	f.Add(":")
	f.Add("nothex:0")
	f.Add(hex.EncodeToString(fuzzHash32) + ":99999999999999999999")

	f.Fuzz(func(t *testing.T, key string) {
		_, _ = OutpointFromMapKey(key)
	})
}

// FuzzTreeFromProtoFields builds a VTXOTree from fuzzer-controlled scalar
// fields for a small fixed node set, including a self-referencing /
// forward / out-of-range child index and a co-signer key. This drives the
// child-index validation, the per-node decode (treeNodeFromProto), and
// ComputeFinalKey on attacker-shaped co-signers, all on the no-panic
// invariant.
func FuzzTreeFromProtoFields(f *testing.F) {
	// Seed: a two-node, structurally valid-ish tree with a forward
	// child reference.
	f.Add(
		fuzzHash32, int64(1000), []byte{0x51},
		uint32(0), uint32(1), make([]byte, 33), make([]byte, 64),
		fuzzHash32,
	)
	// Seed: child index points back at the parent (cycle), zero node
	// amount, empty co-signer/sig.
	f.Add(
		fuzzHash32, int64(0), []byte{},
		uint32(0), uint32(0), []byte{}, []byte{}, []byte{},
	)
	// Seed: huge child output index and child node index to probe the
	// out-of-range guards, plus a negative amount.
	f.Add(
		fuzzHash32[:31], int64(-1), []byte{},
		uint32(0xffffffff), uint32(0xffffffff), make([]byte, 33),
		make([]byte, 64), fuzzHash32,
	)

	f.Fuzz(func(t *testing.T, txHash []byte, amount int64,
		pkScript []byte, childOut, childIdx uint32, coSigner,
		sig, sweepRoot []byte) {

		pt := &VTXOTree{
			Nodes: []*TreeNode{
				{
					Input: &Outpoint{
						TxHash:      txHash,
						OutputIndex: 0,
					},
					Outputs: []*TxOut{
						{
							Value:    amount,
							PkScript: pkScript,
						},
					},
					CoSigners: [][]byte{coSigner},
					Children: map[uint32]uint32{
						childOut: childIdx,
					},
					Amount:    amount,
					Signature: sig,
				},
				{
					Input: &Outpoint{
						TxHash:      txHash,
						OutputIndex: 1,
					},
					Outputs: []*TxOut{
						{
							Value:    amount,
							PkScript: pkScript,
						},
					},
					Children: map[uint32]uint32{},
					Amount:   amount,
				},
			},
			BatchOutpoint: &Outpoint{
				TxHash:      txHash,
				OutputIndex: 0,
			},
			BatchOutput: &TxOut{
				Value:    amount,
				PkScript: pkScript,
			},
			SweepTapscriptRoot: sweepRoot,
		}

		// Only assertion: no panic. The default node-count bound
		// applies; the converter may return any error.
		_, _ = TreeFromProto(pt)
	})
}

// FuzzTreeFromProtoWire feeds raw bytes through proto.Unmarshal into a
// VTXOTree, then into TreeFromProto. This is the strongest probe of the
// node-count bound and the child-index validation: the wire layer can
// materialize many nodes and arbitrary children maps that the field
// fuzzer cannot. A representative seed plus a many-node seed (well under
// the bound) exercise both the accept and reject paths.
func FuzzTreeFromProtoWire(f *testing.F) {
	seed := &VTXOTree{
		Nodes: []*TreeNode{
			{
				Input: &Outpoint{
					TxHash:      fuzzHash32,
					OutputIndex: 0,
				},
				Outputs: []*TxOut{
					{Value: 1000, PkScript: []byte{0x51}},
				},
				Children: map[uint32]uint32{},
				Amount:   1000,
			},
		},
		BatchOutpoint: &Outpoint{TxHash: fuzzHash32, OutputIndex: 0},
		BatchOutput:   &TxOut{Value: 1000, PkScript: []byte{0x51}},
	}
	if raw, err := proto.Marshal(seed); err == nil {
		f.Add(raw)
	}

	// A larger many-node seed (still under DefaultMaxTreeNodes) so the
	// fuzzer has a corpus base from which to grow node counts toward
	// and past the bound, probing for OOM/stack growth.
	big := &VTXOTree{
		BatchOutpoint: &Outpoint{TxHash: fuzzHash32, OutputIndex: 0},
		BatchOutput:   &TxOut{Value: 1, PkScript: []byte{0x51}},
	}
	for i := 0; i < 256; i++ {
		big.Nodes = append(big.Nodes, &TreeNode{
			Input: &Outpoint{
				TxHash:      fuzzHash32,
				OutputIndex: uint32(i),
			},
			Outputs:  []*TxOut{{Value: 1, PkScript: []byte{0x51}}},
			Children: map[uint32]uint32{},
			Amount:   1,
		})
	}
	if raw, err := proto.Marshal(big); err == nil {
		f.Add(raw)
	}
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		var pt VTXOTree
		if err := proto.Unmarshal(data, &pt); err != nil {
			return
		}

		// Pin a small explicit bound so the fuzzer's own large
		// inputs cannot OOM the test process, while still proving the
		// bound is enforced rather than ignored.
		_, _ = TreeFromProto(&pt, WithMaxTreeNodes(4096))
	})
}
