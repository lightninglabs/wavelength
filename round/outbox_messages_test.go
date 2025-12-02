package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/stretchr/testify/require"
)

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
			RoundID:      "round-001",
		}

		result := msg.ToProto()
		require.Nil(t, result)
	})

	t.Run("SubmitNoncesRequest_ToProto", func(t *testing.T) {
		t.Parallel()

		txid := chainhash.HashH([]byte("test-tx"))
		msg := &SubmitNoncesRequest{
			RoundID:        "round-001",
			ParticipantKey: pubKey,
			Nonces: map[chainhash.Hash][]byte{
				txid: {0x01, 0x02},
			},
		}

		result := msg.ToProto()
		require.Nil(t, result)
	})

	t.Run("SubmitPartialSigRequest_ToProto", func(t *testing.T) {
		t.Parallel()

		msg := &SubmitPartialSigRequest{
			RoundID:        "round-001",
			ParticipantKey: pubKey,
			PartialSigs:    [][]byte{{0x01, 0x02}},
		}

		result := msg.ToProto()
		require.Nil(t, result)
	})

	t.Run("SubmitForfeitSigRequest_ToProto", func(t *testing.T) {
		t.Parallel()

		msg := &SubmitForfeitSigRequest{
			RoundID:        "round-001",
			ParticipantKey: pubKey,
			ForfeitSigs:    [][]byte{{0xaa, 0xbb}},
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
		msg := &JoinRoundRequest{RoundID: "round-001"}
		msg.clientOutMsgSealed()
	})

	t.Run("SubmitNoncesRequest", func(t *testing.T) {
		t.Parallel()
		msg := &SubmitNoncesRequest{RoundID: "round-001"}
		msg.clientOutMsgSealed()
	})

	t.Run("SubmitPartialSigRequest", func(t *testing.T) {
		t.Parallel()
		msg := &SubmitPartialSigRequest{RoundID: "round-001"}
		msg.clientOutMsgSealed()
	})

	t.Run("SubmitForfeitSigRequest", func(t *testing.T) {
		t.Parallel()
		msg := &SubmitForfeitSigRequest{RoundID: "round-001"}
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
			RoundID: "round-001",
			TxID:    txid,
		}
		msg.clientOutMsgSealed()
	})

	t.Run("RoundCheckpointedNotification", func(t *testing.T) {
		t.Parallel()
		msg := &RoundCheckpointedNotification{
			RoundID: "round-001",
		}
		msg.clientOutMsgSealed()
	})
}
