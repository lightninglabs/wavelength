package oor

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

// TestOutboxToProtoSubmitRequest verifies that a valid submit package
// outbox request serializes into the expected proto Any envelope.
func TestOutboxToProtoSubmitRequest(t *testing.T) {
	t.Parallel()

	ark, checkpoints := testOutboxPSBTPair(t)

	msg := &SendSubmitPackageRequest{
		ArkPSBT:         ark,
		CheckpointPSBTs: checkpoints,
	}

	protoMsg := msg.ToProto()
	require.IsType(t, &anypb.Any{}, protoMsg)

	anyMsg, ok := protoMsg.(*anypb.Any)
	require.True(t, ok)
	require.Equal(
		t,
		oorOutboxProtoTypeURLPrefix+"SendSubmitPackageRequest",
		anyMsg.TypeUrl,
	)
	require.NotEmpty(t, anyMsg.Value)
}

// TestOutboxToProtoSubmitRequestErrorEnvelope verifies that a submit
// request with missing PSBTs produces an error-typed proto envelope.
func TestOutboxToProtoSubmitRequestErrorEnvelope(t *testing.T) {
	t.Parallel()

	msg := &SendSubmitPackageRequest{}

	protoMsg := msg.ToProto()
	require.IsType(t, &anypb.Any{}, protoMsg)

	anyMsg, ok := protoMsg.(*anypb.Any)
	require.True(t, ok)
	require.Equal(
		t,
		oorOutboxProtoTypeURLPrefix+"SendSubmitPackageRequest.error",
		anyMsg.TypeUrl,
	)
	require.NotEmpty(t, anyMsg.Value)
}

// TestOutboxToProtoFinalizeRequest verifies that a valid finalize
// package outbox request serializes into the expected proto Any
// envelope.
func TestOutboxToProtoFinalizeRequest(t *testing.T) {
	t.Parallel()

	ark, checkpoints := testOutboxPSBTPair(t)

	msg := &SendFinalizePackageRequest{
		ArkPSBT:              ark,
		FinalCheckpointPSBTs: checkpoints,
	}

	protoMsg := msg.ToProto()
	require.IsType(t, &anypb.Any{}, protoMsg)

	anyMsg, ok := protoMsg.(*anypb.Any)
	require.True(t, ok)
	require.Equal(
		t,
		oorOutboxProtoTypeURLPrefix+"SendFinalizePackageRequest",
		anyMsg.TypeUrl,
	)
	require.NotEmpty(t, anyMsg.Value)
}

// TestOutboxToProtoMarkInputsSpentRequest verifies that a mark-inputs-
// spent outbox request serializes into the expected proto Any envelope.
func TestOutboxToProtoMarkInputsSpentRequest(t *testing.T) {
	t.Parallel()

	msg := &MarkInputsSpentRequest{
		Outpoints: []wire.OutPoint{{
			Hash:  chainhash.Hash{1, 2, 3},
			Index: 4,
		}},
	}

	protoMsg := msg.ToProto()
	require.IsType(t, &anypb.Any{}, protoMsg)

	anyMsg, ok := protoMsg.(*anypb.Any)
	require.True(t, ok)
	require.Equal(
		t,
		oorOutboxProtoTypeURLPrefix+"MarkInputsSpentRequest",
		anyMsg.TypeUrl,
	)
	require.NotEmpty(t, anyMsg.Value)
}

// TestOutboxToProtoSendIncomingAckRequest verifies that an incoming-ack
// outbox request serializes into the expected proto Any envelope.
func TestOutboxToProtoSendIncomingAckRequest(t *testing.T) {
	t.Parallel()

	msg := &SendIncomingAckRequest{
		SessionID: SessionID(chainhash.Hash{9, 8, 7}),
	}

	protoMsg := msg.ToProto()
	require.IsType(t, &anypb.Any{}, protoMsg)

	anyMsg, ok := protoMsg.(*anypb.Any)
	require.True(t, ok)
	require.Equal(
		t,
		oorOutboxProtoTypeURLPrefix+"SendIncomingAckRequest",
		anyMsg.TypeUrl,
	)
	require.NotEmpty(t, anyMsg.Value)
}

// TestOutboxToProtoScheduleRetryRequest verifies that a schedule-retry
// outbox request serializes into the expected proto Any envelope.
func TestOutboxToProtoScheduleRetryRequest(t *testing.T) {
	t.Parallel()

	msg := &ScheduleRetryRequest{
		After:  2 * time.Second,
		Reason: "transport timeout",
	}

	protoMsg := msg.ToProto()
	require.IsType(t, &anypb.Any{}, protoMsg)

	anyMsg, ok := protoMsg.(*anypb.Any)
	require.True(t, ok)
	require.Equal(
		t,
		oorOutboxProtoTypeURLPrefix+"ScheduleRetryRequest",
		anyMsg.TypeUrl,
	)
	require.NotEmpty(t, anyMsg.Value)
}

// TestOutboxToProtoScheduleRetryRequestNegativeDelay verifies that a
// schedule-retry request with a negative delay produces an error-typed
// proto envelope.
func TestOutboxToProtoScheduleRetryRequestNegativeDelay(t *testing.T) {
	t.Parallel()

	msg := &ScheduleRetryRequest{
		After:  -1 * time.Second,
		Reason: "transport timeout",
	}

	protoMsg := msg.ToProto()
	require.IsType(t, &anypb.Any{}, protoMsg)

	anyMsg, ok := protoMsg.(*anypb.Any)
	require.True(t, ok)
	require.Equal(
		t,
		oorOutboxProtoTypeURLPrefix+"ScheduleRetryRequest.error",
		anyMsg.TypeUrl,
	)
	require.NotEmpty(t, anyMsg.Value)
}

// testOutboxPSBTPair builds a minimal Ark + checkpoint PSBT pair for
// outbox envelope tests.
func testOutboxPSBTPair(t *testing.T) (*psbt.Packet, []*psbt.Packet) {
	t.Helper()

	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{})
	checkpointTx.AddTxOut(&wire.TxOut{
		Value:    5,
		PkScript: []byte{0x51},
	})
	checkpointPSBT, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)

	arkTx := wire.NewMsgTx(3)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointTx.TxHash(),
			Index: 0,
		},
	})
	arkTx.AddTxOut(&wire.TxOut{Value: 5, PkScript: []byte{0x51}})
	arkTx.AddTxOut(scripts.AnchorOutput())
	arkPSBT, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	return arkPSBT, []*psbt.Packet{checkpointPSBT}
}
