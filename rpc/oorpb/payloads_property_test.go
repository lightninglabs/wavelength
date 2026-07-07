package oorpb

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
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

// genSigningDescriptor generates a random SigningDescriptor.
func genSigningDescriptor(t *rapid.T) SigningDescriptor {
	return SigningDescriptor{
		Outpoint: genOutpoint(t),
		VTXOPolicyTemplate: rapid.SliceOf(
			rapid.Byte(),
		).Draw(t, "policy"),
		SpendPath: rapid.SliceOf(
			rapid.Byte(),
		).Draw(t, "spend_path"),
		OwnerLeafPolicy: rapid.SliceOf(
			rapid.Byte(),
		).Draw(t, "owner_policy"),
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
			t, desc.VTXOPolicyTemplate, got.VTXOPolicyTemplate,
		)
		require.Equal(t, desc.SpendPath, got.SpendPath)
		require.Equal(t, desc.OwnerLeafPolicy, got.OwnerLeafPolicy)
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

		numRecipients := rapid.IntRange(0, 3).Draw(
			rt, "num_recipients",
		)
		recipients := make([]oortx.RecipientOutput, numRecipients)
		for i := range recipients {
			recipientPkScriptLabel := fmt.Sprintf(
				"recipient_pkscript_%d", i)
			recipientValueLabel := fmt.Sprintf("recipient_value_%d",
				i)
			recipientPolicyLabel := fmt.Sprintf(
				"recipient_policy_%d", i)

			recipients[i] = oortx.RecipientOutput{
				PkScript: rapid.SliceOfN(
					rapid.Byte(), 0, 34,
				).Draw(rt, recipientPkScriptLabel),
				Value: btcutil.Amount(
					rapid.Int64Min(0).Draw(
						rt, recipientValueLabel,
					),
				),
				VTXOPolicyTemplate: rapid.SliceOfN(
					rapid.Byte(), 0, 64,
				).Draw(rt, recipientPolicyLabel),
			}
		}

		req, err := NewSubmitPackageRequest(
			ark, checkpoints, descs, recipients,
		)
		require.NoError(t, err)

		gotArk, gotCheckpoints, gotDescs, gotRecipients, err :=
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
				t, descs[i].VTXOPolicyTemplate,
				gotDescs[i].VTXOPolicyTemplate,
			)
			require.Equal(
				t, descs[i].SpendPath, gotDescs[i].SpendPath,
			)
			require.Equal(
				t, descs[i].OwnerLeafPolicy,
				gotDescs[i].OwnerLeafPolicy,
			)
		}

		require.Equal(t, recipients, gotRecipients)
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
		ark := genPSBT(rt)

		resp, err := NewSubmitPackageResponse(
			sessionID, ark, checkpoints,
		)
		require.NoError(t, err)

		gotID, gotArk, gotCheckpoints, err :=
			ParseSubmitPackageResponse(resp)
		require.NoError(t, err)
		require.Equal(t, sessionID, gotID)
		requirePSBTEqual(t, ark, gotArk)
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
