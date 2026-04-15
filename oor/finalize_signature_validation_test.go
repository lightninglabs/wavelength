package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

func encodeFinalScriptWitness(t *testing.T, items ...[]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	err := wire.WriteVarInt(&buf, 0, uint64(len(items)))
	require.NoError(t, err)

	for i := range items {
		err = wire.WriteVarBytes(&buf, 0, items[i])
		require.NoError(t, err)
	}

	return buf.Bytes()
}

func TestParseFinalScriptWitnessRejectsOversizedCount(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := wire.WriteVarInt(&buf, 0, maxFinalWitnessItems+1)
	require.NoError(t, err)

	_, err = parseFinalScriptWitness(buf.Bytes())
	require.ErrorContains(t, err, "exceeds max")
}

func TestParseFinalScriptWitnessRejectsTrailingBytes(t *testing.T) {
	t.Parallel()

	raw := encodeFinalScriptWitness(t, []byte("sig"))
	raw = append(raw, 0x01, 0x02)

	_, err := parseFinalScriptWitness(raw)
	require.ErrorContains(t, err, "trailing bytes")
}

// helperTestInput returns a PInput populated with the provided
// TaprootScriptSpendSig records so findSignature* helpers have
// something to scan over.
func helperTestInputWithSigs(
	sigs ...*psbt.TaprootScriptSpendSig) *psbt.PInput {

	return &psbt.PInput{
		TaprootScriptSpendSig: sigs,
	}
}

// TestFindSignatureByPubKeySingleMatch asserts the happy path.
func TestFindSignatureByPubKeySingleMatch(t *testing.T) {
	t.Parallel()

	op, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	opX := schnorr.SerializePubKey(op.PubKey())

	other, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	otherX := schnorr.SerializePubKey(other.PubKey())

	in := helperTestInputWithSigs(
		&psbt.TaprootScriptSpendSig{
			XOnlyPubKey: otherX,
			LeafHash:    []byte{0x01},
			Signature:   []byte{0xAA},
		},
		&psbt.TaprootScriptSpendSig{
			XOnlyPubKey: opX,
			LeafHash:    []byte{0x02},
			Signature:   []byte{0xBB},
		},
	)

	got, err := findSignatureByPubKey(in, op.PubKey())
	require.NoError(t, err)
	require.Equal(t, []byte{0xBB}, got.Signature)
}

// TestFindSignatureByPubKeyRejectsMultipleForSamePubkey asserts the
// "multi-operator-sig" scenario (same pubkey, two leaves) is
// rejected.
func TestFindSignatureByPubKeyRejectsMultipleForSamePubkey(t *testing.T) {
	t.Parallel()

	op, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	opX := schnorr.SerializePubKey(op.PubKey())

	in := helperTestInputWithSigs(
		&psbt.TaprootScriptSpendSig{
			XOnlyPubKey: opX,
			LeafHash:    []byte{0x01},
			Signature:   []byte{0xAA},
		},
		&psbt.TaprootScriptSpendSig{
			XOnlyPubKey: opX,
			LeafHash:    []byte{0x02},
			Signature:   []byte{0xBB},
		},
	)

	_, err = findSignatureByPubKey(in, op.PubKey())
	require.ErrorContains(t, err, "multiple signatures")
}

// TestFindSignatureByPubKeyMissing asserts a missing operator sig is
// surfaced explicitly rather than silently returning nil.
func TestFindSignatureByPubKeyMissing(t *testing.T) {
	t.Parallel()

	op, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	other, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	otherX := schnorr.SerializePubKey(other.PubKey())

	in := helperTestInputWithSigs(&psbt.TaprootScriptSpendSig{
		XOnlyPubKey: otherX,
		LeafHash:    []byte{0x01},
	})

	_, err = findSignatureByPubKey(in, op.PubKey())
	require.ErrorContains(t, err, "missing signature")
}

// TestFindSignatureByPubKeyAndLeafHash verifies the (pubkey, leafHash)
// pair is required — same pubkey on a different leaf is not
// accepted.
func TestFindSignatureByPubKeyAndLeafHash(t *testing.T) {
	t.Parallel()

	op, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	opX := schnorr.SerializePubKey(op.PubKey())

	in := helperTestInputWithSigs(
		&psbt.TaprootScriptSpendSig{
			XOnlyPubKey: opX,
			LeafHash:    []byte{0x01},
			Signature:   []byte{0xAA},
		},
		&psbt.TaprootScriptSpendSig{
			XOnlyPubKey: opX,
			LeafHash:    []byte{0x02},
			Signature:   []byte{0xBB},
		},
	)

	got, err := findSignatureByPubKeyAndLeafHash(
		in, op.PubKey(), []byte{0x02},
	)
	require.NoError(t, err)
	require.Equal(t, []byte{0xBB}, got.Signature)

	_, err = findSignatureByPubKeyAndLeafHash(
		in, op.PubKey(), []byte{0x03},
	)
	require.ErrorContains(t, err, "missing operator signature")
}

// TestFindSingleNonOperatorSignatureForLeafRejectsMultiple asserts
// the multi-owner-sig case is rejected (the owner may only sign
// once per leaf).
func TestFindSingleNonOperatorSignatureForLeafRejectsMultiple(t *testing.T) {
	t.Parallel()

	op, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	opX := schnorr.SerializePubKey(op.PubKey())

	a, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	aX := schnorr.SerializePubKey(a.PubKey())

	b, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	bX := schnorr.SerializePubKey(b.PubKey())

	leafHash := []byte{0x01}

	in := helperTestInputWithSigs(
		&psbt.TaprootScriptSpendSig{
			XOnlyPubKey: opX,
			LeafHash:    leafHash,
		},
		&psbt.TaprootScriptSpendSig{
			XOnlyPubKey: aX,
			LeafHash:    leafHash,
		},
		&psbt.TaprootScriptSpendSig{
			XOnlyPubKey: bX,
			LeafHash:    leafHash,
		},
	)

	_, err = findSingleNonOperatorSignatureForLeaf(
		in, op.PubKey(), leafHash,
	)
	require.ErrorContains(t, err, "multiple owner signatures")
}

// TestFindSingleNonOperatorSignatureForLeafRequiresOwnerSig asserts
// a missing owner sig is rejected explicitly.
func TestFindSingleNonOperatorSignatureForLeafRequiresOwnerSig(t *testing.T) {
	t.Parallel()

	op, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	opX := schnorr.SerializePubKey(op.PubKey())

	leafHash := []byte{0x01}

	// Only the operator sig is present; the owner sig is absent.
	in := helperTestInputWithSigs(
		&psbt.TaprootScriptSpendSig{
			XOnlyPubKey: opX,
			LeafHash:    leafHash,
		},
	)

	_, err = findSingleNonOperatorSignatureForLeaf(
		in, op.PubKey(), leafHash,
	)
	require.ErrorContains(t, err, "missing owner signature")
}

// TestFindTapLeafByHashMatchAndMiss verifies the tapleaf search
// matches on TapHash and surfaces a miss explicitly.
func TestFindTapLeafByHashMatchAndMiss(t *testing.T) {
	t.Parallel()

	script := []byte{0x51} // OP_1
	leaf := txscript.NewBaseTapLeaf(script)
	leafHash := leaf.TapHash()

	in := &psbt.PInput{
		TaprootLeafScript: []*psbt.TaprootTapLeafScript{{
			Script:       script,
			LeafVersion:  txscript.BaseLeafVersion,
			ControlBlock: []byte{0xC0},
		}},
	}

	got, err := findTapLeafByHash(in, leafHash[:])
	require.NoError(t, err)
	require.Equal(t, script, got.Script)

	// Miss: perturb one byte of the leaf hash.
	wrongHash := chainhash.Hash{}
	copy(wrongHash[:], leafHash[:])
	wrongHash[0] ^= 0xFF

	_, err = findTapLeafByHash(in, wrongHash[:])
	require.ErrorContains(t, err, "tap leaf missing")
}

// TestValidateTapLeafControlBlockBindingDetectsMismatch builds a
// tapleaf whose control block derives to a specific taproot output
// key, then asserts that a prevout with a different pkScript is
// rejected. This is the "control block mismatch" negative case from
// the review's H-9 list.
func TestValidateTapLeafControlBlockBindingDetectsMismatch(t *testing.T) {
	t.Parallel()

	internal, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	script := []byte{0x51} // OP_1
	leaf := txscript.NewBaseTapLeaf(script)
	tree := txscript.AssembleTaprootScriptTree(leaf)

	rootHash := tree.RootNode.TapHash()
	tapKey := txscript.ComputeTaprootOutputKey(
		internal.PubKey(), rootHash[:],
	)
	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	controlBlock := tree.LeafMerkleProofs[0].ToControlBlock(
		internal.PubKey(),
	)
	cbBytes, err := controlBlock.ToBytes()
	require.NoError(t, err)

	tapLeaf := &psbt.TaprootTapLeafScript{
		Script:       script,
		LeafVersion:  txscript.BaseLeafVersion,
		ControlBlock: cbBytes,
	}

	// Match: prevout pkScript equals the derived P2TR.
	err = validateTapLeafControlBlockBinding(
		tapLeaf, &wire.TxOut{PkScript: pkScript},
	)
	require.NoError(t, err)

	// Mismatch: perturb the pkScript so the derived key doesn't
	// match the prevout.
	bogus := make([]byte, len(pkScript))
	copy(bogus, pkScript)
	bogus[len(bogus)-1] ^= 0xFF

	err = validateTapLeafControlBlockBinding(
		tapLeaf, &wire.TxOut{PkScript: bogus},
	)
	require.ErrorContains(t, err, "do not match prevout")
}

// TestParseTaprootScriptSpendSigBytesRawSize accepts the canonical
// 64-byte schnorr sig.
func TestParseTaprootScriptSpendSigBytesRawSize(t *testing.T) {
	t.Parallel()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	msgHash := make([]byte, 32)
	sig, err := schnorr.Sign(priv, msgHash)
	require.NoError(t, err)

	parsed, err := parseTaprootScriptSpendSigBytes(
		sig.Serialize(), txscript.SigHashDefault,
	)
	require.NoError(t, err)
	require.NotNil(t, parsed)
}

// TestParseTaprootScriptSpendSigBytesRejectsWrongSighash verifies
// that a 65-byte signature whose trailing byte doesn't match the
// requested sighash type is rejected.
func TestParseTaprootScriptSpendSigBytesRejectsWrongSighash(t *testing.T) {
	t.Parallel()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	msg := make([]byte, 32)
	sig, err := schnorr.Sign(priv, msg)
	require.NoError(t, err)

	// Append a sighash byte that does NOT match the expected type.
	appended := append(sig.Serialize(), byte(txscript.SigHashAll))

	_, err = parseTaprootScriptSpendSigBytes(
		appended, txscript.SigHashSingle,
	)
	require.Error(t, err)
}

// TestValidateFinalizeCheckpointSignaturesRequiresCoSignedSet
// verifies the outer validator rejects an empty co-signed set.
func TestValidateFinalizeCheckpointSignaturesRequiresCoSignedSet(t *testing.T) {
	t.Parallel()

	op, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	err = validateFinalizeCheckpointSignatures(
		op.PubKey(), nil, []*psbt.Packet{{}},
	)
	require.ErrorContains(t, err, "co-signed checkpoint psbts")
}

// TestValidateFinalizeCheckpointSignaturesRejectsFinalizeWithoutCoSigned
// verifies that a finalized checkpoint whose txid does not appear in
// the co-signed set is rejected. This closes the "client submits an
// unrelated checkpoint at finalize" attack. The check runs before
// per-input signature validation so the test needs no signatures.
func TestValidateFinalizeCheckpointSignaturesRejectsFinalizeWithoutCoSigned(
	t *testing.T) {

	t.Parallel()

	op, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	coSigned := makeTestPSBT(t, 1)
	unrelated := makeTestPSBT(t, 2)

	err = validateFinalizeCheckpointSignatures(
		op.PubKey(), []*psbt.Packet{coSigned},
		[]*psbt.Packet{unrelated},
	)
	require.ErrorContains(t, err, "missing from co-signed set")
}

// TestValidateFinalizeCheckpointSignaturesRequiresFinalizedSet verifies
// the outer validator rejects an empty finalized set.
//
//nolint:ll
func TestValidateFinalizeCheckpointSignaturesRequiresFinalizedSet(t *testing.T) {
	t.Parallel()

	op, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	err = validateFinalizeCheckpointSignatures(
		op.PubKey(), []*psbt.Packet{makeTestPSBT(t, 1)}, nil,
	)
	require.ErrorContains(t, err, "final checkpoint psbts")
}

// TestValidateFinalizeCheckpointSignaturesRequiresOperatorKey verifies
// the outer validator refuses a nil operator key. Without this guard
// an OOR-materialised row with no operator key threaded through would
// skip signature recovery silently.
func TestValidateFinalizeCheckpointSignaturesRequiresOperatorKey(t *testing.T) {
	t.Parallel()

	err := validateFinalizeCheckpointSignatures(
		nil, []*psbt.Packet{makeTestPSBT(t, 1)},
		[]*psbt.Packet{makeTestPSBT(t, 1)},
	)
	require.ErrorContains(t, err, "operator key")
}
