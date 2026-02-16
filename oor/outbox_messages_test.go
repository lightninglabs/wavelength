package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

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
