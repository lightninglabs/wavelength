package treecodec

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestTreeCodecRejectsHugeNumChildren crafts a children blob whose
// numChildren varint claims uint64-max children. The decoder must
// reject this before reaching the make() call so a corrupted durable
// blob cannot OOM the actor on replay.
func TestTreeCodecRejectsHugeNumChildren(t *testing.T) {
	t.Parallel()

	// Hand-roll a deserializeChildren payload: a single varint
	// holding a huge count and no follow-up data.
	payload := []byte{
		// 0xFF prefix tells tlv.ReadVarInt that an 8-byte count
		// follows in big-endian form.
		0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	}

	_, err := deserializeChildren(payload, 2, tree.NewAssetTreeContext())
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds max")
}

// TestTreeCodecNodeAmount retains cached subtree totals for actor snapshots.
// The optional record leaves legacy blobs without a total readable as zero.
func TestTreeCodecNodeAmount(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		amount := rapid.Int64().Draw(rt, "amount")
		original := &tree.Tree{Root: &tree.Node{
			Amount: btcutil.Amount(amount),
		}}
		raw, err := SerializeSnapshot(original)
		require.NoError(rt, err)
		restored, err := DeserializeTree(raw)
		require.NoError(rt, err)
		require.Equal(rt, original.Root.Amount, restored.Root.Amount)
	})
}

// TestTreeCodecLegacyIdentity excludes cached amounts from the existing
// database format because ancestry fragment identities hash these bytes.
func TestTreeCodecLegacyIdentity(t *testing.T) {
	original := &tree.Tree{Root: &tree.Node{Amount: 123}}
	legacy, err := SerializeTree(original)
	require.NoError(t, err)
	snapshot, err := SerializeSnapshot(original)
	require.NoError(t, err)
	require.NotEqual(t, legacy, snapshot)
	original.Root.Amount = 0
	zero, err := SerializeTree(original)
	require.NoError(t, err)
	require.Equal(t, legacy, zero)
}
