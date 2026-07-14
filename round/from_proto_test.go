package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestRoundJoinedFromProto verifies that RoundJoined.FromProto correctly
// populates all fields from a ClientSuccessResp proto message.
func TestRoundJoinedFromProto(t *testing.T) {
	roundID := [16]byte{
		1, 2, 3, 4, 5, 6, 7, 8,
		9, 10, 11, 12, 13, 14, 15, 16,
	}

	boardingOP := wire.OutPoint{
		Hash: chainhash.Hash{
			0xaa,
			0xbb,
		},
		Index: 1,
	}
	vtxoOP := wire.OutPoint{
		Hash: chainhash.Hash{
			0xcc,
			0xdd,
		},
		Index: 2,
	}

	pb := &roundpb.ClientSuccessResp{
		RoundId: roundID[:],
		AcceptedBoardingOutpoints: []*roundpb.Outpoint{
			roundpb.OutpointToProto(boardingOP),
		},
		AcceptedVtxoOutpoints: []*roundpb.Outpoint{
			roundpb.OutpointToProto(vtxoOP),
		},
	}

	var got RoundJoined
	err := got.FromProto(pb)
	require.NoError(t, err)

	require.Equal(t, RoundID(roundID), got.RoundID)
	require.Len(t, got.AcceptedBoardingOutpoints, 1)
	require.Equal(t, boardingOP, got.AcceptedBoardingOutpoints[0])
	require.Len(t, got.AcceptedVTXOOutpoints, 1)
	require.Equal(t, vtxoOP, got.AcceptedVTXOOutpoints[0])
}

// TestRoundJoinedFromProtoRoundTrip verifies marshaling through proto bytes
// preserves all fields.
func TestRoundJoinedFromProtoRoundTrip(t *testing.T) {
	roundID := [16]byte{
		1, 2, 3, 4, 5, 6, 7, 8,
		9, 10, 11, 12, 13, 14, 15, 16,
	}
	boardingOP := wire.OutPoint{
		Hash: chainhash.Hash{
			0xaa,
		},
		Index: 5,
	}

	pb := &roundpb.ClientSuccessResp{
		RoundId: roundID[:],
		AcceptedBoardingOutpoints: []*roundpb.Outpoint{
			roundpb.OutpointToProto(boardingOP),
		},
	}

	// Marshal and unmarshal.
	data, err := proto.Marshal(pb)
	require.NoError(t, err)

	var pb2 roundpb.ClientSuccessResp
	require.NoError(t, proto.Unmarshal(data, &pb2))

	var got RoundJoined
	require.NoError(t, got.FromProto(&pb2))

	require.Equal(t, RoundID(roundID), got.RoundID)
	require.Len(t, got.AcceptedBoardingOutpoints, 1)
	require.Equal(t, boardingOP, got.AcceptedBoardingOutpoints[0])
}

// TestAwaitingBoardingSigsFromProto verifies FromProto on a simple message
// with only a round ID.
func TestAwaitingBoardingSigsFromProto(t *testing.T) {
	roundID := [16]byte{
		10, 20, 30, 40, 50, 60, 70, 80,
		90, 100, 110, 120, 130, 140, 150, 160,
	}

	pb := &roundpb.ClientAwaitingInputSigsResp{
		RoundId: roundID[:],
	}

	var got AwaitingBoardingSigs
	require.NoError(t, got.FromProto(pb))
	require.Equal(t, RoundID(roundID), got.RoundID)
}

// TestJoinRoundRequestFromProtoPreservesVTXOSigningKey verifies the VTXO
// signing pubkey survives the JoinRoundRequest proto decode path.
func TestJoinRoundRequestFromProtoPreservesVTXOSigningKey(t *testing.T) {
	t.Parallel()

	signingPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		clientPriv.PubKey(), operatorPriv.PubKey(), 144,
	)
	require.NoError(t, err)

	pb := &roundpb.JoinRoundRequest{
		VtxoRequests: []*roundpb.VTXORequest{
			{
				TargetAmountSat: 1234,
				PolicyTemplate:  policyTemplate,
				SigningKey: signingPriv.PubKey().
					SerializeCompressed(),
			},
		},
	}

	var got JoinRoundRequest
	err = got.FromProto(pb)
	require.NoError(t, err)
	require.Len(t, got.VTXORequests, 1)
	require.NotNil(t, got.VTXORequests[0].SigningKey.PubKey)
	require.True(
		t,
		got.VTXORequests[0].SigningKey.PubKey.IsEqual(
			signingPriv.PubKey(),
		),
	)
}

// TestJoinRoundRequestFromProtoPreservesCustomForfeitSpendPaths verifies that
// custom refresh spend paths carried by the round wire message survive decode
// into the client round domain type.
func TestJoinRoundRequestFromProtoPreservesCustomForfeitSpendPaths(
	t *testing.T) {

	t.Parallel()

	authSpend := testFromProtoSpendPath(7)
	forfeitSpend := testFromProtoSpendPath(9)

	pb := &roundpb.JoinRoundRequest{
		ForfeitRequests: []*roundpb.ForfeitRequest{{
			VtxoOutpoint: &roundpb.Outpoint{
				TxHash:      make([]byte, 32),
				OutputIndex: 3,
			},
			AuthSpendPath: mustEncodeFromProtoSpendPath(
				t, authSpend,
			),
			ForfeitSpendPath: mustEncodeFromProtoSpendPath(
				t, forfeitSpend,
			),
		}},
	}

	var got JoinRoundRequest
	err := got.FromProto(pb)
	require.NoError(t, err)
	require.Len(t, got.ForfeitRequests, 1)
	require.Equal(
		t, mustEncodeFromProtoSpendPath(t, authSpend),
		mustEncodeFromProtoSpendPath(
			t, got.ForfeitRequests[0].AuthSpend,
		),
	)
	require.Equal(
		t, mustEncodeFromProtoSpendPath(t, forfeitSpend),
		mustEncodeFromProtoSpendPath(
			t, got.ForfeitRequests[0].ForfeitSpend,
		),
	)
}

func testFromProtoSpendPath(sequence uint32) *arkscript.SpendPath {
	return &arkscript.SpendPath{
		RequiredSequence: sequence,
		SpendInfo: &arkscript.SpendInfo{
			WitnessScript: []byte{
				0x51,
				byte(sequence),
			},
			ControlBlock: []byte{
				0xc0,
			},
		},
	}
}

func mustEncodeFromProtoSpendPath(t *testing.T,
	spend *arkscript.SpendPath) []byte {

	t.Helper()

	raw, err := spend.Encode()
	require.NoError(t, err)

	return raw
}

// TestNoncesAggregatedFromProto verifies that NoncesAggregated.FromProto
// correctly parses hex-encoded TxID keys and nonce byte values.
func TestNoncesAggregatedFromProto(t *testing.T) {
	roundID := [16]byte{
		1, 2, 3, 4, 5, 6, 7, 8,
		9, 10, 11, 12, 13, 14, 15, 16,
	}
	txID := tree.TxID{0x01, 0x02, 0x03}

	// Create a 66-byte nonce.
	var nonce tree.Musig2PubNonce
	for i := range nonce {
		nonce[i] = byte(i)
	}

	pb := &roundpb.ClientVTXOAggNonces{
		RoundId: roundID[:],
		AggNonces: map[string][]byte{
			roundpb.TxIDToHex(txID): nonce[:],
		},
	}

	var got NoncesAggregated
	require.NoError(t, got.FromProto(pb))

	require.Equal(t, RoundID(roundID), got.RoundID)
	require.Len(t, got.AggNonces, 1)

	gotNonce, ok := got.AggNonces[txID]
	require.True(t, ok)
	require.Equal(t, nonce, gotNonce)
}

// TestOperatorSignedFromProto verifies that OperatorSigned.FromProto
// correctly parses schnorr signatures from bytes.
func TestOperatorSignedFromProto(t *testing.T) {
	roundID := [16]byte{5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5}
	txID := tree.TxID{0xaa, 0xbb}

	// Create a valid schnorr signature (64 bytes of non-zero data).
	sigBytes := make([]byte, schnorr.SignatureSize)
	for i := range sigBytes {
		sigBytes[i] = byte(i + 1)
	}
	sig, err := schnorr.ParseSignature(sigBytes)
	require.NoError(t, err)

	pb := &roundpb.ClientVTXOAggSigs{
		RoundId: roundID[:],
		AggSigs: map[string][]byte{
			roundpb.TxIDToHex(txID): sig.Serialize(),
		},
	}

	var got OperatorSigned
	require.NoError(t, got.FromProto(pb))

	require.Equal(t, RoundID(roundID), got.RoundID)
	require.Len(t, got.AggSigs, 1)

	gotSig, ok := got.AggSigs[txID]
	require.True(t, ok)
	require.Equal(t, sig.Serialize(), gotSig.Serialize())
}

// TestBoardingFailedFromProtoRoundFailed verifies that BoardingFailed
// handles ClientRoundFailedResp and carries the server-assigned RoundID
// when the proto includes a well-formed 16-byte round_id
// (wavelength#571).
func TestBoardingFailedFromProtoRoundFailed(t *testing.T) {
	roundID := [16]byte{
		1, 2, 3, 4, 5, 6, 7, 8,
		9, 10, 11, 12, 13, 14, 15, 16,
	}

	pb := &roundpb.ClientRoundFailedResp{
		RoundId: roundID[:],
		Reason:  "timeout expired",
	}

	var got BoardingFailed
	require.NoError(t, got.FromProto(pb))
	require.Equal(t, "timeout expired", got.Reason)
	require.True(t, got.Recoverable)
	require.True(
		t, got.RoundID.IsSome(),
		"a 16-byte round_id should populate RoundID",
	)
	require.Equal(t, RoundID(roundID), got.RoundID.UnwrapOr(RoundID{}))
}

// TestBoardingFailedFromProtoRoundFailedNoRoundID verifies that a
// ClientRoundFailedResp without a well-formed 16-byte round_id (a failure
// that arrives before the round was assigned) leaves RoundID as None
// rather than erroring (wavelength#571).
func TestBoardingFailedFromProtoRoundFailedNoRoundID(t *testing.T) {
	pb := &roundpb.ClientRoundFailedResp{
		Reason: "pre-assignment failure",
	}

	var got BoardingFailed
	require.NoError(t, got.FromProto(pb))
	require.Equal(t, "pre-assignment failure", got.Reason)
	require.True(t, got.Recoverable)
	require.True(
		t, got.RoundID.IsNone(),
		"an empty round_id should leave RoundID None",
	)
}

// TestBoardingFailedFromProtoErrorResp verifies that BoardingFailed
// handles ClientErrorResp (the other proto type that maps to this event).
// ClientErrorResp has no round_id, so RoundID stays None.
func TestBoardingFailedFromProtoErrorResp(t *testing.T) {
	pb := &roundpb.ClientErrorResp{
		ErrorMsg: "internal error",
	}

	var got BoardingFailed
	require.NoError(t, got.FromProto(pb))
	require.Equal(t, "internal error", got.Reason)
	require.True(t, got.Recoverable)
	require.True(
		t, got.RoundID.IsNone(),
		"ClientErrorResp carries no round_id",
	)
}

// TestFromProtoWrongType verifies that FromProto returns an error for
// unexpected proto types.
func TestFromProtoWrongType(t *testing.T) {
	wrongMsg := &roundpb.ClientErrorResp{ErrorMsg: "wrong"}

	var joined RoundJoined
	err := joined.FromProto(wrongMsg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unexpected proto type")
}

// TestFromProtoInvalidRoundID verifies that FromProto rejects invalid
// round ID lengths.
func TestFromProtoInvalidRoundID(t *testing.T) {
	pb := &roundpb.ClientSuccessResp{
		RoundId: []byte{
			1,
			2,
			3,
		}, // Too short.
	}

	var joined RoundJoined
	err := joined.FromProto(pb)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid round_id length")
}

// testRoundKey returns a deterministic valid public key for the given seed
// byte, used to exercise the round-delivered signing-key fields.
func testRoundKey(t *testing.T, seed byte) *btcec.PublicKey {
	t.Helper()

	var buf [32]byte
	buf[31] = seed
	_, pub := btcec.PrivKeyFromBytes(buf[:])

	return pub
}

// TestCommitmentTxBuiltFromProtoSigningKeys verifies that FromProto parses
// the per-round tree-cosign and connector operator keys, leaves them nil when
// the server omits them (older-server fallback path), and rejects a malformed
// key.
func TestCommitmentTxBuiltFromProtoSigningKeys(t *testing.T) {
	t.Parallel()

	roundID := [16]byte{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
	}
	treeKey := testRoundKey(t, 0x02)
	connKey := testRoundKey(t, 0x03)

	// Both keys present: parsed onto the event.
	pb := &roundpb.ClientBatchInfo{
		RoundId:              roundID[:],
		BatchPsbt:            validPSBTBytes(t),
		TreeCosignKey:        treeKey.SerializeCompressed(),
		ConnectorOperatorKey: connKey.SerializeCompressed(),
	}
	var got CommitmentTxBuilt
	require.NoError(t, got.FromProto(pb))
	require.NotNil(t, got.TreeCosignKey)
	require.True(t, treeKey.IsEqual(got.TreeCosignKey))
	require.NotNil(t, got.ConnectorOperatorKey)
	require.True(t, connKey.IsEqual(got.ConnectorOperatorKey))

	// Keys absent: left nil so the FSM falls back to the global operator
	// key (wire compatibility with a server that predates the fields).
	pbEmpty := &roundpb.ClientBatchInfo{
		RoundId:   roundID[:],
		BatchPsbt: validPSBTBytes(t),
	}
	var gotEmpty CommitmentTxBuilt
	require.NoError(t, gotEmpty.FromProto(pbEmpty))
	require.Nil(t, gotEmpty.TreeCosignKey)
	require.Nil(t, gotEmpty.ConnectorOperatorKey)

	// Malformed key: rejected rather than silently dropped.
	pbBad := &roundpb.ClientBatchInfo{
		RoundId:   roundID[:],
		BatchPsbt: validPSBTBytes(t),
		TreeCosignKey: []byte{
			0x00,
			0x01,
			0x02,
		},
	}
	var gotBad CommitmentTxBuilt
	require.Error(t, gotBad.FromProto(pbBad))
}
