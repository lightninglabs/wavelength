package tree

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// recordingMuSig2Signer wraps the mock signer and captures the taproot
// tweak of every created session, so tests can assert which tweak each
// node was signed with.
type recordingMuSig2Signer struct {
	*mockMuSig2Signer

	tweaks [][]byte
}

// MuSig2CreateSession records the session's taproot tweak before
// delegating to the wrapped mock.
func (r *recordingMuSig2Signer) MuSig2CreateSession(version input.MuSig2Version,
	keyLoc keychain.KeyLocator, signers []*btcec.PublicKey,
	tweaks *input.MuSig2Tweaks, otherNonces [][musig2.PubNonceSize]byte,
	localNonces *musig2.Nonces) (*input.MuSig2SessionInfo, error) {

	r.tweaks = append(
		r.tweaks,
		append(
			[]byte(nil), tweaks.TaprootTweak...,
		),
	)

	return r.mockMuSig2Signer.MuSig2CreateSession(
		version, keyLoc, signers, tweaks, otherNonces, localNonces,
	)
}

// containsTweak reports whether the recorded tweaks include the given one.
func containsTweak(recorded [][]byte, want []byte) bool {
	for _, tweak := range recorded {
		if bytes.Equal(tweak, want) {
			return true
		}
	}

	return false
}

// newTweakTestPath builds a two-node root→leaf path where the given signer
// cosigns every node, returning the root and the outpoints identifying each
// node for lookup keying.
func newTweakTestPath(t *testing.T, pubKey *btcec.PublicKey) (*Node,
	wire.OutPoint, wire.OutPoint) {

	t.Helper()

	leaf := createSimpleLeaf(
		"tweak-leaf", 1_000, []*btcec.PublicKey{pubKey},
	)
	root := createSimpleLeaf(
		"tweak-root", 1_000, []*btcec.PublicKey{pubKey},
	)
	root.Children = map[uint32]*Node{0: leaf}

	rootTXID, err := root.TXID()
	require.NoError(t, err)
	leaf.Input = wire.OutPoint{Hash: rootTXID, Index: 0}

	return root, root.Input, leaf.Input
}

// TestSignerSessionTweakLookup ensures a tweak lookup overrides the signing
// tweak per node, keyed by the node's input outpoint (path extraction clones
// nodes, so pointer identity cannot be used), and that nodes the lookup does
// not cover fall back to the sweep tapscript root.
func TestSignerSessionTweakLookup(t *testing.T) {
	t.Parallel()

	privKey, pubKey := createTestKey(t)
	sweepRoot := bytes.Repeat([]byte{0x01}, 32)
	rootTweak := bytes.Repeat([]byte{0xAA}, 32)
	leafTweak := bytes.Repeat([]byte{0xBB}, 32)

	t.Run("lookup covers every node", func(t *testing.T) {
		t.Parallel()

		root, rootOutpoint, leafOutpoint := newTweakTestPath(t, pubKey)
		tweaksByInput := map[wire.OutPoint][]byte{
			rootOutpoint: rootTweak,
			leafOutpoint: leafTweak,
		}

		signer := &recordingMuSig2Signer{
			mockMuSig2Signer: newMockMuSig2Signer(privKey),
		}
		fetcher, err := root.PrevOutputFetcher(
			&wire.TxOut{
				Value: 5_000,
			},
		)
		require.NoError(t, err)

		session, err := NewSignerSession(
			signer, &keychain.KeyDescriptor{PubKey: pubKey},
			sweepRoot, fetcher, root,
			func(node *Node) []byte {
				return tweaksByInput[node.Input]
			},
		)
		require.NoError(t, err)
		require.Len(t, session.SessionIDs(), 2)

		require.Len(t, signer.tweaks, 2)
		require.True(t, containsTweak(signer.tweaks, rootTweak))
		require.True(t, containsTweak(signer.tweaks, leafTweak))
		require.False(t, containsTweak(signer.tweaks, sweepRoot))
	})

	t.Run("uncovered node falls back to sweep root", func(t *testing.T) {
		t.Parallel()

		root, rootOutpoint, _ := newTweakTestPath(t, pubKey)
		tweaksByInput := map[wire.OutPoint][]byte{
			rootOutpoint: rootTweak,
		}

		signer := &recordingMuSig2Signer{
			mockMuSig2Signer: newMockMuSig2Signer(privKey),
		}
		fetcher, err := root.PrevOutputFetcher(
			&wire.TxOut{
				Value: 5_000,
			},
		)
		require.NoError(t, err)

		_, err = NewSignerSession(
			signer, &keychain.KeyDescriptor{PubKey: pubKey},
			sweepRoot, fetcher, root,
			func(node *Node) []byte {
				return tweaksByInput[node.Input]
			},
		)
		require.NoError(t, err)

		require.Len(t, signer.tweaks, 2)
		require.True(t, containsTweak(signer.tweaks, rootTweak))
		require.True(t, containsTweak(signer.tweaks, sweepRoot))
	})
}

// TestComputeInternalKey pins the relationship between the untweaked
// aggregate key and the tweaked final key: applying the taproot tweak to
// the internal key must yield exactly ComputeFinalKey's result, since tapd
// derives anchor output keys from the internal key we hand it.
func TestComputeInternalKey(t *testing.T) {
	t.Parallel()

	_, err := ComputeInternalKey(nil)
	require.ErrorContains(t, err, "no cosigners")

	_, single := createTestKey(t)
	got, err := ComputeInternalKey([]*btcec.PublicKey{single})
	require.NoError(t, err)
	require.True(t, single.IsEqual(got))

	cosigners := make([]*btcec.PublicKey, 3)
	for i := range cosigners {
		_, cosigners[i] = createTestKey(t)
	}
	tweak := bytes.Repeat([]byte{0x42}, 32)

	internal, err := ComputeInternalKey(cosigners)
	require.NoError(t, err)
	final, err := ComputeFinalKey(cosigners, tweak)
	require.NoError(t, err)

	derived := txscript.ComputeTaprootOutputKey(internal, tweak)
	require.Equal(
		t, final.SerializeCompressed()[1:],
		derived.SerializeCompressed()[1:],
	)
}
