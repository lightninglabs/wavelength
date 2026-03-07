package oorpb

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// ---------------------------------------------------------------------------
// Rapid generators
// ---------------------------------------------------------------------------

// genHash generates a random 32-byte chainhash.Hash.
func genHash(t *rapid.T) chainhash.Hash {
	var h chainhash.Hash
	b := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "hash")
	copy(h[:], b)

	return h
}

// genOutpoint generates a random wire.OutPoint.
func genOutpoint(t *rapid.T) wire.OutPoint {
	return wire.OutPoint{
		Hash:  genHash(t),
		Index: rapid.Uint32().Draw(t, "index"),
	}
}

// genPubKey generates a random secp256k1 public key.
func genPubKey(t *rapid.T) *btcec.PublicKey {
	privBytes := rapid.SliceOfN(
		rapid.Byte(), 32, 32,
	).Draw(t, "priv_key")
	privKey, _ := btcec.PrivKeyFromBytes(privBytes)
	if privKey == nil {
		privKey, _ = btcec.NewPrivateKey()
	}

	return privKey.PubKey()
}

// genSigningDescriptor generates a random SigningDescriptor.
func genSigningDescriptor(t *rapid.T) SigningDescriptor {
	return SigningDescriptor{
		Outpoint:  genOutpoint(t),
		OwnerKey:  genPubKey(t),
		ExitDelay: rapid.Uint32().Draw(t, "exit_delay"),
	}
}

// genPSBT generates a minimal valid PSBT with random input/output.
func genPSBT(t *rapid.T) *psbt.Packet {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: genOutpoint(t),
	})

	maxSats := int64(21_000_000_00000000)
	tx.AddTxOut(&wire.TxOut{
		Value:    rapid.Int64Range(1, maxSats).Draw(t, "value"),
		PkScript: []byte{txscript.OP_TRUE},
	})

	pkt, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		// Should never happen with a valid MsgTx.
		panic(err)
	}

	return pkt
}

// ---------------------------------------------------------------------------
// Property tests
// ---------------------------------------------------------------------------

// TestOutPointProtoRoundTrip verifies that any wire.OutPoint survives
// encode → decode through OOR proto outpoint representation.
func TestOutPointProtoRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		op := genOutpoint(rt)
		pb := encodeOutPoint(op)

		got, err := decodeOutPoint(pb)
		require.NoError(t, err)
		require.Equal(t, op, got)
	})
}

// TestSigningDescriptorRoundTrip verifies that any SigningDescriptor
// survives encode → decode through proto representation.
func TestSigningDescriptorRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		desc := genSigningDescriptor(rt)
		pb, err := encodeSigningDescriptor(desc, 0)
		require.NoError(t, err)

		got, err := decodeSigningDescriptor(pb, 0)
		require.NoError(t, err)
		require.Equal(t, desc.Outpoint, got.Outpoint)
		require.Equal(
			t,
			desc.OwnerKey.SerializeCompressed(),
			got.OwnerKey.SerializeCompressed(),
		)
		require.Equal(t, desc.ExitDelay, got.ExitDelay)
	})
}

// TestSubmitPackageRequestRoundTripProperty verifies that random
// submit package requests survive NewSubmitPackageRequest →
// ParseSubmitPackageRequest round-trip.
func TestSubmitPackageRequestRoundTripProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		ark := genPSBT(rt)

		numCheckpoints := rapid.IntRange(0, 3).Draw(
			rt, "num_checkpoints",
		)
		checkpoints := make([]*psbt.Packet, numCheckpoints)
		for i := range checkpoints {
			checkpoints[i] = genPSBT(rt)
		}

		numDescs := rapid.IntRange(0, 3).Draw(
			rt, "num_descs",
		)
		descs := make([]SigningDescriptor, numDescs)
		for i := range descs {
			descs[i] = genSigningDescriptor(rt)
		}

		req, err := NewSubmitPackageRequest(
			ark, checkpoints, descs,
		)
		require.NoError(t, err)

		gotArk, gotCheckpoints, gotDescs, err :=
			ParseSubmitPackageRequest(req)
		require.NoError(t, err)

		// Verify Ark PSBT.
		requirePSBTEqual(t, ark, gotArk)

		// Verify checkpoints.
		require.Equal(
			t, len(checkpoints), len(gotCheckpoints),
		)
		for i := range checkpoints {
			requirePSBTEqual(t, checkpoints[i], gotCheckpoints[i])
		}

		// Verify signing descriptors.
		require.Equal(t, len(descs), len(gotDescs))
		for i := range descs {
			require.Equal(
				t, descs[i].Outpoint, gotDescs[i].Outpoint,
			)
			require.Equal(
				t,
				descs[i].OwnerKey.SerializeCompressed(),
				gotDescs[i].OwnerKey.SerializeCompressed(),
			)
			require.Equal(
				t, descs[i].ExitDelay, gotDescs[i].ExitDelay,
			)
		}
	})
}

// TestSubmitPackageResponseRoundTripProperty verifies that random
// submit package responses survive New → Parse round-trip.
func TestSubmitPackageResponseRoundTripProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		sessionID := genHash(rt)

		numCheckpoints := rapid.IntRange(0, 3).Draw(
			rt, "num_checkpoints",
		)
		checkpoints := make([]*psbt.Packet, numCheckpoints)
		for i := range checkpoints {
			checkpoints[i] = genPSBT(rt)
		}

		resp, err := NewSubmitPackageResponse(
			sessionID, checkpoints,
		)
		require.NoError(t, err)

		gotID, gotCheckpoints, err := ParseSubmitPackageResponse(
			resp,
		)
		require.NoError(t, err)
		require.Equal(t, sessionID, gotID)
		require.Equal(
			t, len(checkpoints), len(gotCheckpoints),
		)

		for i := range checkpoints {
			requirePSBTEqual(t, checkpoints[i], gotCheckpoints[i])
		}
	})
}

// TestFinalizePackageRequestRoundTripProperty verifies that random
// finalize package requests survive New → Parse round-trip.
func TestFinalizePackageRequestRoundTripProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		sessionID := genHash(rt)

		numCheckpoints := rapid.IntRange(0, 3).Draw(
			rt, "num_checkpoints",
		)
		checkpoints := make([]*psbt.Packet, numCheckpoints)
		for i := range checkpoints {
			checkpoints[i] = genPSBT(rt)
		}

		req, err := NewFinalizePackageRequest(
			sessionID, checkpoints,
		)
		require.NoError(t, err)

		gotID, gotCheckpoints, err := ParseFinalizePackageRequest(
			req,
		)
		require.NoError(t, err)
		require.Equal(t, sessionID, gotID)
		require.Equal(
			t, len(checkpoints), len(gotCheckpoints),
		)

		for i := range checkpoints {
			requirePSBTEqual(t, checkpoints[i], gotCheckpoints[i])
		}
	})
}

// TestFinalizePackageResponseRoundTripProperty verifies that random
// finalize package responses survive New → Parse round-trip.
func TestFinalizePackageResponseRoundTripProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		sessionID := genHash(rt)

		resp := NewFinalizePackageResponse(sessionID)

		gotID, err := ParseFinalizePackageResponse(resp)
		require.NoError(t, err)
		require.Equal(t, sessionID, gotID)
	})
}

// TestDecodeSessionIDRejectsInvalidLength verifies that session IDs
// with non-32 byte lengths are rejected.
func TestDecodeSessionIDRejectsInvalidLength(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		// Generate a byte slice that is NOT 32 bytes long.
		length := rapid.IntRange(0, 100).Draw(rt, "length")
		if length == chainhash.HashSize {
			length++
		}

		b := rapid.SliceOfN(
			rapid.Byte(), length, length,
		).Draw(rt, "bytes")

		_, err := decodeSessionID(b)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid session id length")
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// requirePSBTEqual asserts two PSBTs are byte-identical when serialized.
func requirePSBTEqual(t testing.TB, a, b *psbt.Packet) {
	t.Helper()

	aRaw, err := psbtutil.Serialize(a)
	require.NoError(t, err)

	bRaw, err := psbtutil.Serialize(b)
	require.NoError(t, err)

	require.True(t, bytes.Equal(aRaw, bRaw))
}
