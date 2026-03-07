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
)

// TestSubmitPackageRequestRoundTrip verifies submit request conversion through
// typed proto payloads.
func TestSubmitPackageRequestRoundTrip(t *testing.T) {
	t.Parallel()

	ark := mustTestPSBT(t, 11)
	checkpoints := []*psbt.Packet{
		mustTestPSBT(t, 21),
		mustTestPSBT(t, 22),
	}

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	hashHex := "0f555f77697777895555121212121212" +
		"12121212121212121212121212121212"
	descs := []SigningDescriptor{{
		Outpoint: wire.OutPoint{
			Hash:  mustHash(t, hashHex),
			Index: 7,
		},
		OwnerKey:  priv.PubKey(),
		ExitDelay: 144,
	}}

	req, err := NewSubmitPackageRequest(ark, checkpoints, descs)
	require.NoError(t, err)

	decArk, decCheckpoints, decDescs, err := ParseSubmitPackageRequest(req)
	require.NoError(t, err)
	require.Equal(t, 1, len(decDescs))
	require.Equal(t, descs[0].Outpoint, decDescs[0].Outpoint)
	require.Equal(
		t,
		descs[0].OwnerKey.SerializeCompressed(),
		decDescs[0].OwnerKey.SerializeCompressed(),
	)
	require.Equal(t, descs[0].ExitDelay, decDescs[0].ExitDelay)

	require.True(
		t,
		bytes.Equal(
			mustSerializePSBT(t, ark),
			mustSerializePSBT(t, decArk),
		),
	)
	require.Equal(t, len(checkpoints), len(decCheckpoints))
	for i := range checkpoints {
		require.True(
			t,
			bytes.Equal(
				mustSerializePSBT(t, checkpoints[i]),
				mustSerializePSBT(t, decCheckpoints[i]),
			),
		)
	}
}

// TestSubmitPackageResponseRoundTrip verifies submit response conversion
// through typed proto payloads.
func TestSubmitPackageResponseRoundTrip(t *testing.T) {
	t.Parallel()

	submitSessionIDHex := "8f555f77697777895555121212121212" +
		"12121212121212121212121212121212"
	sessionID := mustHash(
		t,
		submitSessionIDHex,
	)
	checkpoints := []*psbt.Packet{
		mustTestPSBT(t, 31),
		mustTestPSBT(t, 32),
	}

	resp, err := NewSubmitPackageResponse(sessionID, checkpoints)
	require.NoError(t, err)

	decSessionID, decCheckpoints, err := ParseSubmitPackageResponse(resp)
	require.NoError(t, err)
	require.Equal(t, sessionID, decSessionID)
	require.Equal(t, len(checkpoints), len(decCheckpoints))

	for i := range checkpoints {
		require.True(
			t,
			bytes.Equal(
				mustSerializePSBT(t, checkpoints[i]),
				mustSerializePSBT(t, decCheckpoints[i]),
			),
		)
	}
}

// TestFinalizePackageRoundTrip verifies finalize request/response conversion
// through typed proto payloads.
func TestFinalizePackageRoundTrip(t *testing.T) {
	t.Parallel()

	finalizeSessionIDHex := "af555f77697777895555121212121212" +
		"12121212121212121212121212121212"
	sessionID := mustHash(
		t,
		finalizeSessionIDHex,
	)
	finalCheckpoints := []*psbt.Packet{
		mustTestPSBT(t, 41),
	}

	req, err := NewFinalizePackageRequest(sessionID, finalCheckpoints)
	require.NoError(t, err)

	decSessionID, decFinalCheckpoints, err := ParseFinalizePackageRequest(
		req,
	)
	require.NoError(t, err)
	require.Equal(t, sessionID, decSessionID)
	require.Equal(t, len(finalCheckpoints), len(decFinalCheckpoints))
	require.True(
		t,
		bytes.Equal(
			mustSerializePSBT(t, finalCheckpoints[0]),
			mustSerializePSBT(t, decFinalCheckpoints[0]),
		),
	)

	resp := NewFinalizePackageResponse(sessionID)
	respSessionID, err := ParseFinalizePackageResponse(resp)
	require.NoError(t, err)
	require.Equal(t, sessionID, respSessionID)
}

// TestParseFinalizePackageResponseRejectsInvalidSessionLength verifies session
// id validation on typed finalize responses.
func TestParseFinalizePackageResponseRejectsInvalidSessionLength(t *testing.T) {
	t.Parallel()

	_, err := ParseFinalizePackageResponse(&FinalizePackageResponse{
		SessionId: []byte{1, 2, 3},
	})
	require.ErrorContains(t, err, "invalid session id length")
}

// mustHash parses a chain hash string for tests.
func mustHash(t *testing.T, hash string) chainhash.Hash {
	t.Helper()

	parsed, err := chainhash.NewHashFromStr(hash)
	require.NoError(t, err)

	return *parsed
}

// mustTestPSBT builds a minimal serializable PSBT packet for tests.
func mustTestPSBT(t *testing.T, marker byte) *psbt.Packet {
	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash: chainhash.Hash{marker},
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	return packet
}

// mustSerializePSBT serializes a PSBT packet in tests.
func mustSerializePSBT(t *testing.T, packet *psbt.Packet) []byte {
	t.Helper()

	raw, err := psbtutil.Serialize(packet)
	require.NoError(t, err)

	return raw
}
