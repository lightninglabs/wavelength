package rounds

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// genTestKey returns a freshly generated secp256k1 pubkey for use in tests.
func genTestKey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv.PubKey()
}

// makeStandardVTXODescriptor produces a VTXODescriptor for the standard
// (owner/operator/expiry) VTXO shape with the given signing (co-signer) key
// and amount. The PkScript is derived from (ownerKey, operatorKey,
// exitDelay) — independent of the signing key — so callers can construct
// multiple descriptors that share a PkScript by reusing the same owner/
// operator/expiry triple.
func makeStandardVTXODescriptor(t *testing.T, ownerKey, operatorKey,
	signingKey *btcec.PublicKey, exitDelay uint32,
	amount btcutil.Amount) *tree.VTXODescriptor {

	t.Helper()

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		ownerKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	outputKey, err := arkscript.VTXOTapKey(
		ownerKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(outputKey)
	require.NoError(t, err)

	return &tree.VTXODescriptor{
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
		Amount:         amount,
		CoSignerKey:    signingKey,
	}
}

// vertexFromPubKey serializes a pubkey into a SigningKeyHex (route.Vertex).
func vertexFromPubKey(pk *btcec.PublicKey) SigningKeyHex {
	var v route.Vertex
	copy(v[:], pk.SerializeCompressed())

	return v
}

// TestCollectVTXOsDuplicateScripts verifies that collectVTXOs preserves
// per-leaf descriptor metadata (Amount, CoSignerKey) when two VTXO requests
// produce the same PkScript (same owner/operator/expiry policy) but have
// distinct signing keys and amounts.
//
// This is a regression test for issue #365: the original PkScript-keyed
// descriptor index let the second descriptor silently overwrite the first,
// so both leaves were persisted with the second descriptor's amount/
// signing-key metadata. Refresh and forfeit accounting later read the
// corrupted amount and either under-credited the VTXO or rejected its
// collaborative spend.
func TestCollectVTXOsDuplicateScripts(t *testing.T) {
	t.Parallel()

	// Use a single owner key + operator key + expiry. Both descriptors
	// share these inputs, so they compile to identical PkScripts.
	operatorKey := genTestKey(t)
	ownerKey := genTestKey(t)
	const exitDelay uint32 = 144

	// Two distinct signing (cosigner) keys — what ValidateVTXORequest
	// enforces uniqueness on. Same owner/operator/expiry means the two
	// descriptors collide on PkScript.
	signingKeyA := genTestKey(t)
	signingKeyB := genTestKey(t)

	const (
		amountA = btcutil.Amount(7_500)
		amountB = btcutil.Amount(12_500)
	)

	descA := makeStandardVTXODescriptor(
		t, ownerKey, operatorKey, signingKeyA, exitDelay, amountA,
	)
	descB := makeStandardVTXODescriptor(
		t, ownerKey, operatorKey, signingKeyB, exitDelay, amountB,
	)

	// Sanity: PkScripts collide; signing keys do not.
	require.True(
		t, bytes.Equal(descA.PkScript, descB.PkScript),
		"test setup: descriptors must share PkScript to exercise "+
			"the duplicate-script path",
	)
	require.False(
		t,
		bytes.Equal(
			signingKeyA.SerializeCompressed(),
			signingKeyB.SerializeCompressed(),
		),
		"test setup: signing keys must differ",
	)

	// Build a real two-leaf VTXO tree from the two colliding descriptors.
	sweepKey := genTestKey(t)
	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("collect-vtxos-batch")),
		Index: 0,
	}
	batchOutputAmt := int64(amountA + amountB)
	batchPkScript, err := txscript.PayToTaprootScript(operatorKey)
	require.NoError(t, err)
	batchOutput := wire.NewTxOut(batchOutputAmt, batchPkScript)

	vtxoTree, err := tree.BuildVTXOTree(
		batchOutpoint, batchOutput,
		[]tree.VTXODescriptor{*descA, *descB}, operatorKey, sweepKey,
		exitDelay, 2,
	)
	require.NoError(t, err)

	// Register the descriptors against a single client, keyed by their
	// unique signing keys (mirroring the production registration shape).
	clientRegs := map[clientconn.ClientID]*ClientRegistration{
		"client-1": {
			ClientID: "client-1",
			VTXODescriptors: map[SigningKeyHex]*tree.VTXODescriptor{
				vertexFromPubKey(signingKeyA): descA,
				vertexFromPubKey(signingKeyB): descB,
			},
		},
	}

	const batchOutputIdx = 0
	var roundID RoundID
	roundIDSeed := chainhash.HashH([]byte("collect-vtxos-round"))
	copy(roundID[:], roundIDSeed[:])

	vtxos, err := collectVTXOs(
		roundID, operatorKey, map[int]*tree.Tree{
			batchOutputIdx: vtxoTree,
		},
		clientRegs,
	)
	require.NoError(t, err)
	require.Len(t, vtxos, 2,
		"expected one persisted VTXO per tree leaf")

	// Bucket the persisted VTXOs by signing key and assert each kept
	// its own (Amount, CoSignerKey). Pre-fix, both VTXOs would carry
	// whichever descriptor was iterated last from the map — corrupting
	// at least one leaf's Amount.
	bySigningKey := make(map[string]*VTXO)
	for _, v := range vtxos {
		require.NotNil(t, v.Descriptor)
		require.NotNil(t, v.Descriptor.CoSignerKey)
		key := string(v.Descriptor.CoSignerKey.SerializeCompressed())
		_, dup := bySigningKey[key]
		require.False(
			t, dup, "two persisted VTXOs share the same "+
				"signing key — descriptor index collapsed "+
				"colliding leaves",
		)
		bySigningKey[key] = v
	}

	keyAStr := string(signingKeyA.SerializeCompressed())
	keyBStr := string(signingKeyB.SerializeCompressed())

	vtxoA, okA := bySigningKey[keyAStr]
	require.True(t, okA, "missing persisted VTXO for signing key A")
	vtxoB, okB := bySigningKey[keyBStr]
	require.True(t, okB, "missing persisted VTXO for signing key B")

	require.Equal(
		t, amountA, vtxoA.Descriptor.Amount,
		"persisted amount for leaf A must match its descriptor",
	)
	require.Equal(
		t, amountB, vtxoB.Descriptor.Amount,
		"persisted amount for leaf B must match its descriptor",
	)

	// Both VTXOs should still share the PkScript (the collision is
	// real on-chain; the fix only disambiguates the descriptor lookup).
	require.True(
		t, bytes.Equal(
			vtxoA.Descriptor.PkScript, vtxoB.Descriptor.PkScript,
		),
		"persisted leaves should still share the colliding PkScript",
	)

	// Outpoints must be distinct (sanity).
	require.NotEqual(t, vtxoA.Outpoint, vtxoB.Outpoint)
}

// TestCollectVTXOsRejectsNilOperatorKey guards the precondition that the
// operator key — required to disambiguate the leaf's owner cosigner — is
// supplied by the caller.
func TestCollectVTXOsRejectsNilOperatorKey(t *testing.T) {
	t.Parallel()

	_, err := collectVTXOs(
		RoundID{}, nil, map[int]*tree.Tree{},
		map[clientconn.ClientID]*ClientRegistration{},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil operator key")
}
