package recovery

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestEncodeProofNilRejected verifies the top-level guard.
func TestEncodeProofNilRejected(t *testing.T) {
	_, err := EncodeProof(nil)
	require.ErrorContains(t, err, "proof cannot be nil")
}

// TestProofCodecRoundTrip exercises a few hand-built proofs to catch
// regressions on the fixture shapes we care about in practice.
func TestProofCodecRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		build func(*testing.T) *Proof
	}{
		{
			name: "single_node",
			build: func(t *testing.T) *Proof {
				tx := makeProofTx('a', nil)
				p, err := NewProof(
					wire.OutPoint{
						Hash: tx.TxHash(),
					},
					10, &Node{
						Kind: NodeKindArk,
						Tx:   tx,
					},
				)
				require.NoError(t, err)

				return p
			},
		},
		{
			name: "linear_chain",
			build: func(t *testing.T) *Proof {
				root := makeProofTx('r', nil)
				mid := makeProofTx('m', []wire.OutPoint{
					{Hash: root.TxHash(), Index: 0},
				})
				target := makeProofTx('t', []wire.OutPoint{
					{Hash: mid.TxHash(), Index: 0},
				})

				p, err := NewProof(
					wire.OutPoint{
						Hash: target.TxHash(),
					},
					5,
					&Node{
						Kind: NodeKindCheckpoint,
						Tx:   root,
					},
					&Node{
						Kind: NodeKindCheckpoint,
						Tx:   mid,
					},
					&Node{
						Kind: NodeKindArk, Tx: target,
					},
				)
				require.NoError(t, err)

				return p
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			proof := tc.build(t)

			raw, err := EncodeProof(proof)
			require.NoError(t, err)

			decoded, err := DecodeProof(raw)
			require.NoError(t, err)

			require.Equal(
				t, proof.TargetOutpoint(),
				decoded.TargetOutpoint(),
			)
			require.Equal(t,
				proof.CSVDelay(),
				decoded.CSVDelay())

			// Structural equality: same layer count and same
			// txid set per layer.
			orig := proof.Layers()
			back := decoded.Layers()
			require.Equal(t, len(orig), len(back))
			for i := range orig {
				require.ElementsMatch(t, orig[i], back[i])
			}

			// Re-encoding must yield identical bytes.
			raw2, err := EncodeProof(decoded)
			require.NoError(t, err)
			require.True(
				t, bytes.Equal(raw, raw2),
				"proof encoding must be canonical",
			)
		})
	}
}

// TestProofCodecVersionMismatch verifies unknown versions are rejected.
func TestProofCodecVersionMismatch(t *testing.T) {
	tx := makeProofTx('x', nil)
	proof, err := NewProof(
		wire.OutPoint{
			Hash: tx.TxHash(),
		},
		3, &Node{
			Kind: NodeKindTree,
			Tx:   tx,
		},
	)
	require.NoError(t, err)

	raw, err := EncodeProof(proof)
	require.NoError(t, err)

	// The first TLV is the version record; its payload byte lives at
	// offset 2 (type byte + length byte + value).
	require.GreaterOrEqual(t, len(raw), 3)
	raw[2] = 42

	_, err = DecodeProof(raw)
	require.ErrorContains(t, err, "unsupported proof codec")
}

// TestDecodeProofRejectsInvalidKind verifies a tampered NodeKind byte fails
// loudly rather than silently mapping to an unknown kind. We build a
// well-formed per-Node nested TLV stream carrying an out-of-range kind byte
// and feed it through the outer list framing.
func TestDecodeProofRejectsInvalidKind(t *testing.T) {
	tx := makeProofTx('a', nil)
	badNode := encodeNodeFrame(t, NodeKindArk+50, tx)

	buf := bytes.Buffer{}
	buf.Write([]byte{0, 0, 0, 1}) // count=1
	writeLen(&buf, len(badNode))
	buf.Write(badNode)

	_, err := decodeProofNodes(buf.Bytes())
	require.ErrorContains(t, err, "invalid node kind")
}

// TestDecodeProofRejectsDuplicateTxid verifies a blob that encodes the same
// transaction twice is rejected by the decoder even though the encoder would
// never emit such a blob.
func TestDecodeProofRejectsDuplicateTxid(t *testing.T) {
	tx := makeProofTx('a', nil)
	nodeBytes := encodeNodeFrame(t, NodeKindArk, tx)

	buf := bytes.Buffer{}
	buf.Write([]byte{0, 0, 0, 2}) // count=2

	for i := 0; i < 2; i++ {
		writeLen(&buf, len(nodeBytes))
		buf.Write(nodeBytes)
	}

	_, err := decodeProofNodes(buf.Bytes())
	require.ErrorContains(t, err, "duplicate proof node")
}

// encodeNodeFrame is a test helper that produces the same nested TLV
// sub-stream a Node would serialize to, but lets us inject arbitrary kind
// values for adversarial-input tests.
func encodeNodeFrame(t *testing.T, kind NodeKind, tx *wire.MsgTx) []byte {
	t.Helper()

	raw, err := encodeNodeStream(&Node{Kind: kind, Tx: tx})
	require.NoError(t, err)

	return raw
}

// writeLen appends a 4-byte big-endian length prefix to buf. Extracted so
// the test fixture builders stay readable.
func writeLen(buf *bytes.Buffer, n int) {
	buf.Write([]byte{
		byte(n >> 24), byte(n >> 16),
		byte(n >> 8), byte(n),
	})
}

// TestProofCodecRapidRoundTrip generates random proofs (linear chains of
// varying depth) and asserts round-trip equivalence for the structural
// invariants callers rely on. Every generated proof is guaranteed to
// round-trip exactly; a failure shrinks to the minimum-size counterexample.
func TestProofCodecRapidRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		depth := rapid.IntRange(1, 6).Draw(t, "depth")
		csvDelay := rapid.Uint32Range(
			0, MaxCSVDelay,
		).Draw(t, "csvDelay")

		var nodes []*Node
		var prevTxid chainhash.Hash
		for i := 0; i < depth; i++ {
			var prevOuts []wire.OutPoint
			if i > 0 {
				prevOuts = []wire.OutPoint{
					{
						Hash:  prevTxid,
						Index: 0,
					},
				}
			}
			tag := byte(i + 1)
			tx := makeProofTx(tag, prevOuts)
			prevTxid = tx.TxHash()

			kind := NodeKind(
				rapid.IntRange(
					int(NodeKindTree), int(NodeKindArk),
				).Draw(t, fmt.Sprintf("kind-%d", i)),
			)

			nodes = append(nodes, &Node{Kind: kind, Tx: tx})
		}

		proof, err := NewProof(
			wire.OutPoint{
				Hash:  prevTxid,
				Index: 0,
			},
			csvDelay,
			nodes...,
		)
		if err != nil {
			t.Fatalf("NewProof failed: %v", err)
		}

		raw, err := EncodeProof(proof)
		if err != nil {
			t.Fatalf("EncodeProof failed: %v", err)
		}

		decoded, err := DecodeProof(raw)
		if err != nil {
			t.Fatalf("DecodeProof failed: %v", err)
		}

		if proof.TargetOutpoint() != decoded.TargetOutpoint() {
			t.Fatal("target outpoint mismatch")
		}
		if proof.CSVDelay() != decoded.CSVDelay() {
			t.Fatal("csv delay mismatch")
		}

		orig := proof.Layers()
		back := decoded.Layers()
		if len(orig) != len(back) {
			t.Fatal("layer count mismatch")
		}
		for i := range orig {
			if len(orig[i]) != len(back[i]) {
				t.Fatal("layer size mismatch")
			}
			for _, txid := range orig[i] {
				found := false
				for _, b := range back[i] {
					if txid == b {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("missing txid %s", txid)
				}
			}
		}

		raw2, err := EncodeProof(decoded)
		if err != nil {
			t.Fatalf("re-encode failed: %v", err)
		}
		if !bytes.Equal(raw, raw2) {
			t.Fatalf("encoding is not canonical")
		}
	})
}

// makeProofTx constructs a deterministic MsgTx for codec tests. Mirrors
// makeRecoveryTx in recovery_test.go but lives here so the codec tests are
// self-contained.
func makeProofTx(tag byte, prevOuts []wire.OutPoint) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	if len(prevOuts) == 0 {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{tag, 0xff},
				Index: uint32(tag),
			},
			Sequence: wire.MaxTxInSequenceNum,
		})
	}
	for _, op := range prevOuts {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: op,
			Sequence:         wire.MaxTxInSequenceNum,
		})
	}
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(tag) + 1,
		PkScript: []byte{0x51, tag},
	})

	return tx
}
