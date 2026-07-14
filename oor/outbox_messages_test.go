package oor

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/rpc/oorpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

// TestOutboxToProtoSubmitRequest verifies that a valid submit package
// outbox request serializes into the expected proto message.
func TestOutboxToProtoSubmitRequest(t *testing.T) {
	t.Parallel()

	ark, checkpoints := testOutboxPSBTPair(t)

	msg := &SendSubmitPackageRequest{
		ArkPSBT:         ark,
		CheckpointPSBTs: checkpoints,
	}

	result := msg.ToProto().UnwrapOrFail(t)

	_, ok := result.(*oorpb.SubmitPackageRequest)
	require.True(t, ok)
}

// TestOutboxToProtoSubmitRequestError verifies that a submit request
// with missing PSBTs returns an error via the Result type.
func TestOutboxToProtoSubmitRequestError(t *testing.T) {
	t.Parallel()

	msg := &SendSubmitPackageRequest{}

	_, err := msg.ToProto().Unpack()
	require.Error(t, err)
}

// TestOutboxToProtoFinalizeRequest verifies that a valid finalize
// package outbox request serializes into the expected proto message.
func TestOutboxToProtoFinalizeRequest(t *testing.T) {
	t.Parallel()

	ark, checkpoints := testOutboxPSBTPair(t)

	msg := &SendFinalizePackageRequest{
		ArkPSBT:              ark,
		FinalCheckpointPSBTs: checkpoints,
	}

	result := msg.ToProto().UnwrapOrFail(t)

	_, ok := result.(*oorpb.FinalizePackageRequest)
	require.True(t, ok)
}

// TestOutboxToProtoMarkInputsSpentRequest verifies that a mark-inputs-
// spent outbox request serializes into the expected proto Any envelope.
func TestOutboxToProtoMarkInputsSpentRequest(t *testing.T) {
	t.Parallel()

	msg := &MarkInputsSpentRequest{
		Outpoints: []wire.OutPoint{{
			Hash: chainhash.Hash{
				1,
				2,
				3,
			},
			Index: 4,
		}},
	}

	result := msg.ToProto().UnwrapOrFail(t)

	anyMsg, ok := result.(*anypb.Any)
	require.True(t, ok)
	require.Equal(
		t, oorOutboxProtoTypeURLPrefix+"MarkInputsSpentRequest",
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

	result := msg.ToProto().UnwrapOrFail(t)

	anyMsg, ok := result.(*anypb.Any)
	require.True(t, ok)
	require.Equal(
		t, oorOutboxProtoTypeURLPrefix+"SendIncomingAckRequest",
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

	result := msg.ToProto().UnwrapOrFail(t)

	anyMsg, ok := result.(*anypb.Any)
	require.True(t, ok)
	require.Equal(
		t, oorOutboxProtoTypeURLPrefix+"ScheduleRetryRequest",
		anyMsg.TypeUrl,
	)
	require.NotEmpty(t, anyMsg.Value)
}

// TestOutboxToProtoScheduleRetryRequestNegativeDelay verifies that a
// schedule-retry request with a negative delay returns an error via
// the Result type.
func TestOutboxToProtoScheduleRetryRequestNegativeDelay(t *testing.T) {
	t.Parallel()

	msg := &ScheduleRetryRequest{
		After:  -1 * time.Second,
		Reason: "transport timeout",
	}

	_, err := msg.ToProto().Unpack()
	require.Error(t, err)
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
	arkTx.AddTxOut(arkscript.AnchorOutput())
	arkPSBT, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	return arkPSBT, []*psbt.Packet{checkpointPSBT}
}
