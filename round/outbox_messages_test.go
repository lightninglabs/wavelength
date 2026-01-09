package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// testRoundIDForMsg creates a deterministic RoundID from a string seed.
func testRoundIDForMsg(seed string) RoundID {
	h := chainhash.HashH([]byte(seed))
	id, _ := uuid.FromBytes(h[:16])

	return RoundID(id)
}

// TestOutboxMessagesToProto ensures that ToProto() methods compile and return
// the expected nil placeholders. These placeholders will be replaced with
// actual proto marshaling once the proto definitions are finalized, but this
// test prevents accidental breakage of the interface contract in the meantime.
func TestOutboxMessagesToProto(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKey := privKey.PubKey()

	t.Run("JoinRoundRequest_ToProto", func(t *testing.T) {
		t.Parallel()

		msg := &JoinRoundRequest{
			BoardingRequests: []types.BoardingRequest{
				{ClientKey: pubKey, OperatorKey: pubKey},
			},
			VTXORequests: []types.VTXORequest{},
		}

		result := msg.ToProto()
		require.Nil(t, result)
	})

	t.Run("SubmitNoncesRequest_ToProto", func(t *testing.T) {
		t.Parallel()

		txid := chainhash.HashH([]byte("test-tx"))
		signerKey := NewSignerKey(pubKey)
		var nonce tree.Musig2PubNonce
		copy(nonce[:], []byte{0x01, 0x02})

		msg := &SubmitNoncesRequest{
			RoundID: testRoundIDForMsg("round-001"),
			Nonces: map[SignerKey]map[tree.TxID]tree.Musig2PubNonce{
				signerKey: {
					txid: nonce,
				},
			},
		}

		result := msg.ToProto()
		require.Nil(t, result)
	})

	t.Run("SubmitPartialSigRequest_ToProto", func(t *testing.T) {
		t.Parallel()

		fakeTxid := chainhash.HashH([]byte("test-tx"))
		signerKey := NewSignerKey(pubKey)

		// Create a test partial signature.
		var scalar btcec.ModNScalar
		scalar.SetInt(12345)
		partialSig := &musig2.PartialSignature{S: &scalar}

		msg := &SubmitPartialSigRequest{
			RoundID: testRoundIDForMsg("round-001"),
			Signatures: map[SignerKey]map[tree.TxID]*musig2.PartialSignature{ //nolint:ll
				signerKey: {
					fakeTxid: partialSig,
				},
			},
		}

		result := msg.ToProto()
		require.Nil(t, result)
	})

	t.Run("SubmitForfeitSigRequest_ToProto", func(t *testing.T) {
		t.Parallel()

		msg := &SubmitForfeitSigRequest{
			RoundID: testRoundIDForMsg("round-001"),
			Signatures: []*types.BoardingInputSignature{
				{
					InputIndex: 0,
					Outpoint:   wire.OutPoint{},
				},
			},
		}

		result := msg.ToProto()
		require.Nil(t, result)
	})
}

// TestOutboxMessagesClientOutMsgSealed ensures that all outbox message types
// implement the ClientOutMsg sealed interface. The clientOutMsgSealed() method
// acts as a compile-time marker preventing external types from implementing
// ClientOutMsg, so this test verifies the marker exists on all expected types.
func TestOutboxMessagesClientOutMsgSealed(t *testing.T) {
	t.Parallel()

	txid := chainhash.HashH([]byte("test-txid"))

	t.Run("JoinRoundRequest", func(t *testing.T) {
		t.Parallel()
		msg := &JoinRoundRequest{}
		msg.clientOutMsgSealed()
	})

	t.Run("SubmitNoncesRequest", func(t *testing.T) {
		t.Parallel()
		roundID := testRoundIDForMsg("round-001")
		msg := &SubmitNoncesRequest{RoundID: roundID}
		msg.clientOutMsgSealed()
	})

	t.Run("SubmitPartialSigRequest", func(t *testing.T) {
		t.Parallel()
		roundID := testRoundIDForMsg("round-001")
		msg := &SubmitPartialSigRequest{RoundID: roundID}
		msg.clientOutMsgSealed()
	})

	t.Run("SubmitForfeitSigRequest", func(t *testing.T) {
		t.Parallel()
		roundID := testRoundIDForMsg("round-001")
		msg := &SubmitForfeitSigRequest{RoundID: roundID}
		msg.clientOutMsgSealed()
	})

	t.Run("RegisterConfirmationRequest", func(t *testing.T) {
		t.Parallel()
		msg := &RegisterConfirmationRequest{
			CallerID:    "caller-001",
			PkScript:    []byte{0x00, 0x14},
			TargetConfs: 6,
		}
		msg.clientOutMsgSealed()
	})

	t.Run("VTXOCreatedNotification", func(t *testing.T) {
		t.Parallel()
		msg := &VTXOCreatedNotification{
			VTXOs: []*ClientVTXO{},
		}
		msg.clientOutMsgSealed()
	})

	t.Run("RoundCompletedNotification", func(t *testing.T) {
		t.Parallel()
		msg := &RoundCompletedNotification{
			RoundID: testRoundIDForMsg("round-001"),
			TxID:    txid,
		}
		msg.clientOutMsgSealed()
	})

	t.Run("RoundCheckpointedNotification", func(t *testing.T) {
		t.Parallel()
		msg := &RoundCheckpointedNotification{
			RoundID: testRoundIDForMsg("round-001"),
		}
		msg.clientOutMsgSealed()
	})

	t.Run("RoundFailedNotification", func(t *testing.T) {
		t.Parallel()
		msg := &RoundFailedNotification{
			RoundID:       fn.Some(testRoundIDForMsg("round-001")),
			Reason:        "validation failed",
			Recoverable:   true,
			OriginalError: nil,
		}
		msg.clientOutMsgSealed()
	})
}
