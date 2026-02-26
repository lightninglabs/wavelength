package assets

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestCloneTxOut verifies deep copy behavior of cloneTxOut.
func TestCloneTxOut(t *testing.T) {
	original := &wire.TxOut{
		Value:    12345,
		PkScript: []byte{0x00, 0x14, 0xab, 0xcd},
	}

	cloned := cloneTxOut(original)

	// Values should match.
	require.Equal(t, original.Value, cloned.Value)
	require.Equal(t, original.PkScript, cloned.PkScript)

	// Modifying clone should not affect original.
	cloned.Value = 99999
	cloned.PkScript[0] = 0xff

	require.Equal(t, int64(12345), original.Value)
	require.Equal(t, byte(0x00), original.PkScript[0])

	// Nil input should return nil.
	require.Nil(t, cloneTxOut(nil))
}

// TestCloneTxOuts verifies deep copy behavior of cloneTxOuts.
func TestCloneTxOuts(t *testing.T) {
	original := []*wire.TxOut{
		{Value: 100, PkScript: []byte{0x01}},
		{Value: 200, PkScript: []byte{0x02}},
	}

	cloned := cloneTxOuts(original)

	require.Len(t, cloned, 2)
	require.Equal(t, original[0].Value, cloned[0].Value)
	require.Equal(t, original[1].Value, cloned[1].Value)

	// Modifying clone should not affect original.
	cloned[0].Value = 999

	require.Equal(t, int64(100), original[0].Value)

	// Empty slice should return nil.
	require.Nil(t, cloneTxOuts(nil))
	require.Nil(t, cloneTxOuts([]*wire.TxOut{}))
}

// TestCloneTaprootLeafScripts verifies deep copy of taproot leaf scripts.
func TestCloneTaprootLeafScripts(t *testing.T) {
	original := []*psbt.TaprootTapLeafScript{
		{
			LeafVersion:  0xc0,
			ControlBlock: []byte{0x01, 0x02, 0x03},
			Script:       []byte{0x51}, // OP_TRUE
		},
		{
			LeafVersion:  0xc0,
			ControlBlock: []byte{0x04, 0x05},
			Script:       []byte{0x52}, // OP_2
		},
	}

	cloned := cloneTaprootLeafScripts(original)

	require.Len(t, cloned, 2)
	require.Equal(t, original[0].LeafVersion, cloned[0].LeafVersion)
	require.Equal(t, original[0].ControlBlock, cloned[0].ControlBlock)
	require.Equal(t, original[0].Script, cloned[0].Script)

	// Modifying clone should not affect original.
	cloned[0].ControlBlock[0] = 0xff
	cloned[0].Script[0] = 0xff

	require.Equal(t, byte(0x01), original[0].ControlBlock[0])
	require.Equal(t, byte(0x51), original[0].Script[0])

	// Empty/nil input.
	require.Nil(t, cloneTaprootLeafScripts(nil))
	require.Nil(t, cloneTaprootLeafScripts([]*psbt.TaprootTapLeafScript{}))
}

// TestCloneTaprootBip32 verifies deep copy of BIP32 derivation paths.
func TestCloneTaprootBip32(t *testing.T) {
	original := []*psbt.TaprootBip32Derivation{{
		XOnlyPubKey:          []byte{0x01, 0x02},
		MasterKeyFingerprint: 0x12345678,
		Bip32Path:            []uint32{84, 0, 0, 0, 0},
		LeafHashes: [][]byte{
			{0xaa, 0xbb}, {0xcc, 0xdd},
		},
	}}

	cloned := cloneTaprootBip32(original)

	require.Len(t, cloned, 1)
	require.Equal(t, original[0].XOnlyPubKey, cloned[0].XOnlyPubKey)
	require.Equal(t, original[0].MasterKeyFingerprint,
		cloned[0].MasterKeyFingerprint)
	require.Equal(t, original[0].Bip32Path, cloned[0].Bip32Path)
	require.Equal(t, original[0].LeafHashes, cloned[0].LeafHashes)

	// Modifying clone should not affect original.
	cloned[0].XOnlyPubKey[0] = 0xff
	cloned[0].Bip32Path[0] = 999
	cloned[0].LeafHashes[0][0] = 0xff

	require.Equal(t, byte(0x01), original[0].XOnlyPubKey[0])
	require.Equal(t, uint32(84), original[0].Bip32Path[0])
	require.Equal(t, byte(0xaa), original[0].LeafHashes[0][0])
}

// TestCloneBip32 verifies deep copy of standard BIP32 derivation.
func TestCloneBip32(t *testing.T) {
	original := []*psbt.Bip32Derivation{
		{
			PubKey:               []byte{0x02, 0x03},
			MasterKeyFingerprint: 0xabcdef01,
			Bip32Path:            []uint32{44, 0, 0, 0, 0},
		},
	}

	cloned := cloneBip32(original)

	require.Len(t, cloned, 1)
	require.Equal(t, original[0].PubKey, cloned[0].PubKey)

	// Modifying clone should not affect original.
	cloned[0].PubKey[0] = 0xff

	require.Equal(t, byte(0x02), original[0].PubKey[0])
}

// TestCloneBtcInputPlan verifies deep copy of BtcInputPlan.
func TestCloneBtcInputPlan(t *testing.T) {
	original := BtcInputPlan{
		Description: "test input",
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x01, 0x02},
			Index: 5,
		},
		Sequence: 0xfffffffd,
		WitnessUtxo: &wire.TxOut{
			Value:    50000,
			PkScript: []byte{0x00, 0x14, 0xab},
		},
	}

	cloned := cloneBtcInputPlan(original)

	require.Equal(t, original.Description, cloned.Description)
	require.Equal(t, original.Outpoint, cloned.Outpoint)
	require.Equal(t, original.Sequence, cloned.Sequence)
	require.Equal(t, original.WitnessUtxo.Value, cloned.WitnessUtxo.Value)

	// Modifying clone should not affect original.
	cloned.WitnessUtxo.Value = 99999
	cloned.WitnessUtxo.PkScript[0] = 0xff

	require.Equal(t, int64(50000), original.WitnessUtxo.Value)
	require.Equal(t, byte(0x00), original.WitnessUtxo.PkScript[0])
}

// TestCloneBtcOutputPlan verifies deep copy of BtcOutputPlan.
func TestCloneBtcOutputPlan(t *testing.T) {
	original := BtcOutputPlan{
		Description: "test output",
		ValueSat:    100000,
		PkScript:    []byte{0x51, 0x20, 0xab},
		OutputIndex: 2,
	}

	cloned := cloneBtcOutputPlan(original)

	require.Equal(t, original.Description, cloned.Description)
	require.Equal(t, original.ValueSat, cloned.ValueSat)
	require.Equal(t, original.PkScript, cloned.PkScript)
	require.Equal(t, original.OutputIndex, cloned.OutputIndex)

	// Modifying clone should not affect original.
	cloned.PkScript[0] = 0xff

	require.Equal(t, byte(0x51), original.PkScript[0])
}

// TestCopyWitness verifies deep copy of transaction witness.
func TestCopyWitness(t *testing.T) {
	original := wire.TxWitness{
		[]byte{0x30, 0x44}, // signature
		[]byte{0x02, 0x21}, // pubkey
	}

	copied := copyWitness(original)

	require.Len(t, copied, 2)
	require.Equal(t, original[0], copied[0])
	require.Equal(t, original[1], copied[1])

	// Modifying copy should not affect original.
	copied[0][0] = 0xff

	require.Equal(t, byte(0x30), original[0][0])
}

// TestTapBranchHashBytes verifies lexicographic sorting in branch hash.
func TestTapBranchHashBytes(t *testing.T) {
	left := []byte{0x01, 0x02, 0x03}
	right := []byte{0x04, 0x05, 0x06}

	// Hash should be the same regardless of input order.
	hash1 := tapBranchHashBytes(left, right)
	hash2 := tapBranchHashBytes(right, left)

	require.Equal(t, hash1, hash2)

	// Hash should be deterministic.
	hash3 := tapBranchHashBytes(left, right)
	require.Equal(t, hash1, hash3)
}

// TestValidateAnchorKeyMuSig2 verifies MuSig2 anchor key validation.
func TestValidateAnchorKeyMuSig2(t *testing.T) {
	// Valid MuSig2 spec.
	validSpec := AnchorKeySpec{
		Mode: AnchorKeyModeMuSig2,
		MuSig2: &MuSig2Spec{
			Participants: []MuSig2Participant{
				{
					Role: "user",
					// compressed pubkey
					PubKey: make([]byte, 33),
				},
				{
					Role:   "operator",
					PubKey: make([]byte, 33),
				},
			},
		},
	}

	err := validateAnchorKey(validSpec)
	require.NoError(t, err)

	// Missing MuSig2 spec.
	invalidSpec := AnchorKeySpec{
		Mode: AnchorKeyModeMuSig2,
	}
	err = validateAnchorKey(invalidSpec)
	require.Error(t, err)
	require.Contains(t, err.Error(), "musig2 specification missing")

	// Empty participants.
	invalidSpec = AnchorKeySpec{
		Mode: AnchorKeyModeMuSig2,
		MuSig2: &MuSig2Spec{
			Participants: []MuSig2Participant{},
		},
	}
	err = validateAnchorKey(invalidSpec)
	require.Error(t, err)
	require.Contains(t, err.Error(), "musig2 participants empty")

	// Invalid pubkey length.
	invalidSpec = AnchorKeySpec{
		Mode: AnchorKeyModeMuSig2,
		MuSig2: &MuSig2Spec{
			Participants: []MuSig2Participant{
				{
					Role: "user",
					// wrong size
					PubKey: make([]byte, 32),
				},
			},
		},
	}
	err = validateAnchorKey(invalidSpec)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pubkey must be 33 bytes")
}

// TestValidateAnchorKeyStatic verifies static anchor key validation.
func TestValidateAnchorKeyStatic(t *testing.T) {
	// Valid static spec.
	validSpec := AnchorKeySpec{
		Mode: AnchorKeyModeStatic,
		Key:  make([]byte, 32), // x-only pubkey
	}

	err := validateAnchorKey(validSpec)
	require.NoError(t, err)

	// Invalid key length.
	invalidSpec := AnchorKeySpec{
		Mode: AnchorKeyModeStatic,
		Key:  make([]byte, 33), // wrong size
	}
	err = validateAnchorKey(invalidSpec)
	require.Error(t, err)
	require.Contains(t, err.Error(), "static anchor key must be 32 bytes")
}
