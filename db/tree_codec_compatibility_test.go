package db

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/treecodec"
	"github.com/stretchr/testify/require"
)

// compatibilityTree returns a deterministic pre-extraction database fixture.
// Nonzero sweep policy, cached keys and subtree amounts distinguish each codec.
func compatibilityTree(t *testing.T) *tree.Tree {
	t.Helper()
	private, key := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{17}, 32))
	signature, err := schnorr.Sign(private, bytes.Repeat([]byte{19}, 32))
	require.NoError(t, err)
	point := wire.OutPoint{Hash: chainhash.Hash{1, 2}, Index: 3}
	child := &tree.Node{Input: wire.OutPoint{
		Hash: chainhash.Hash{
			4,
		},
		Index: 1,
	},
		Outputs: []*wire.TxOut{
			wire.NewTxOut(400, []byte{0x51}),
		}, Amount: 400,
		CoSigners: []*btcec.PublicKey{
			key,
		}, FinalKey: key, Signature: signature}

	return &tree.Tree{BatchOutpoint: point, BatchOutput: wire.NewTxOut(
		500, []byte{0x51, 0x52},
	),
		SweepTapscriptRoot: bytes.Repeat([]byte{23},
			32),
		SweepKey:   key,
		SweepDelay: 144,
		Root: &tree.Node{Input: point, Outputs: []*wire.TxOut{
			wire.NewTxOut(400, []byte{0x51}),
		},
			CoSigners: []*btcec.PublicKey{
				key,
			}, Amount: 500, FinalKey: key, Signature: signature,
			Children: map[uint32]*tree.Node{
				0: child,
			}}}
}

// TestSharedTreeCodecLegacyBytes pins database, fragment and backfill encodings
// captured from the original codec at the prerequisite head. Actor-only cached
// amounts must never change persisted proof or ancestry-identity bytes.
func TestSharedTreeCodecLegacyBytes(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		encode func(*tree.Tree) ([]byte, error)
		digest string
	}{
		{
			"database",
			SerializeTree,
			"8716db5b3159b79042bd6d0ae08240d6" +
				"51e66799290a4596f6d0f25ecb5dc915",
		},
		{
			"fragment",
			treecodec.SerializeFragment,
			"44222b1087d22c1d3eb8405ec1d1cfad" +
				"51486f411ecc278e4549fb9f7a5ac4ee",
		},
		{
			"backfill",
			treecodec.SerializeLegacyFragment,
			"95d6f0283830d454f5ef8d9f8f64e52a" +
				"e3b816cd3916970ed0419c3572bc3828",
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			raw, err := fixture.encode(compatibilityTree(t))
			require.NoError(t, err)
			require.Equal(
				t, fixture.digest,
				fmt.Sprintf(
					"%x", sha256.Sum256(raw),
				),
			)
		})
	}
}
