package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestReceiveSessionNotifiesThenQueriesMetadata verifies the incoming-transfer
// FSM emits notification and metadata-query outbox work first, deferring ack
// until local materialization is confirmed.
func TestReceiveSessionNotifiesThenQueriesMetadata(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// This test exercises the client-side incoming-transfer FSM.
	//
	// We construct an Ark PSBT that looks like a canonical v0 transfer
	// (checkpoint input -> recipients + anchor), then verify the receive
	// session:
	// - emits an application-facing notification
	// - requests authoritative incoming metadata
	// Ack is emitted only after materialization is confirmed.
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	exitDelay := uint32(10)

	inputValue := btcutil.Amount(10000)

	inputs := []oortx.CheckpointInput{
		{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: wire.OutPoint{
					Hash: [32]byte{
						0x01,
					},
					Index: 0,
				},
				Output: &wire.TxOut{
					Value: int64(inputValue),
					PkScript: newTestTaprootPkScript(
						t, operatorKey.PubKey(),
					),
				},
			},
			OwnerLeafScript: []byte{
				0x51,
			},
		},
	}

	vtxoTapKey, err := arkscript.VTXOTapKey(
		recipientKey.PubKey(), policy.OperatorKey, exitDelay,
	)
	require.NoError(t, err)

	recipientPkScript, err := txscript.PayToTaprootScript(vtxoTapKey)
	require.NoError(t, err)

	outputs := []oortx.RecipientOutput{
		{
			PkScript: recipientPkScript,
			Value:    inputValue,
		},
	}

	// Build a canonical Ark PSBT for the receive notification.
	//
	// The checkpoint PSBT is only used to derive a realistic Ark input:
	// we are not testing checkpoint validity here, only the receive FSM's
	// structural checks and outbox emission.
	cp, err := oortx.BuildCheckpointPSBT(policy, inputs[0])
	require.NoError(t, err)

	arkPSBT, err := oortx.BuildArkPSBT(
		[]oortx.CheckpointOutput{
			{
				Txid:           cp.PSBT.UnsignedTx.TxHash(),
				Output:         cp.PSBT.UnsignedTx.TxOut[0],
				TapTreeEncoded: cp.TapTreeEncoded,
			},
		},
		outputs,
	)
	require.NoError(t, err)

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())

	_, outbox, err := DriveIncomingTransfer(ctx, sessionID, arkPSBT)
	require.NoError(t, err)

	// The transition emits the UI notification, the metadata query, and a
	// give-up timer armed alongside the query (no failure response on
	// operator silence).
	require.Len(t, outbox, 3)

	_, ok := outbox[0].(*IncomingTransferNotification)
	require.True(t, ok)

	queryMsg, ok := outbox[1].(*QueryIncomingMetadataRequest)
	require.True(t, ok)
	require.NotEmpty(t, queryMsg.Recipients)

	_, ok = outbox[2].(*ScheduleRetryRequest)
	require.True(t, ok)
	parentCommitment := inputs[0].SpentVTXO.Outpoint.Hash

	desc, err := BuildIncomingVTXODescriptor(queryMsg.ArkPSBT,
		IncomingVTXOConfig{
			OutputIndex: queryMsg.Recipients[0].OutputIndex,
			ClientKey: keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			},
			OperatorKey: policy.OperatorKey,
			ExitDelay:   exitDelay,
			Metadata: IncomingVTXOMetadata{
				CommitmentTxID: parentCommitment,
				RoundID:        "round-test",
				BatchExpiry:    100,
				Ancestry: validTestIncomingAncestry(
					parentCommitment,
				),
				CreatedHeight: 50,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, recipientPkScript, desc.PkScript)
	require.Equal(t, inputValue, desc.Amount)
	require.Equal(t, vtxo.VTXOStatusLive, desc.Status)
}

// TestReceiveSessionAcksAfterHandled asserts ack is emitted only after the
// application confirms materialization completion.
func TestReceiveSessionAcksAfterHandled(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkPSBT, _, _, _, _, _ := buildTestIncomingMaterialization(t)
	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())

	sess, outbox, err := DriveIncomingTransfer(ctx, sessionID, arkPSBT)
	require.NoError(t, err)

	// Notification, metadata query, and the give-up timer armed alongside
	// the query.
	require.Len(t, outbox, 3)

	fut := sess.FSM.AskEvent(ctx, &IncomingMetadataResolvedEvent{
		Matches: []IncomingMetadataMatch{{
			OutputIndex: 0,
			Metadata: IncomingVTXOMetadata{
				RoundID: "round-test",
			},
		}},
	})
	result := fut.Await(ctx)
	require.False(t, result.IsErr())

	materializeOutbox := result.UnwrapOr(nil)
	require.Len(t, materializeOutbox, 1)
	require.IsType(
		t, &MaterializeIncomingVTXOsRequest{}, materializeOutbox[0],
	)

	fut = sess.FSM.AskEvent(ctx, &IncomingHandledEvent{})
	result = fut.Await(ctx)
	require.False(t, result.IsErr())

	ackOutbox := result.UnwrapOr(nil)
	require.Len(t, ackOutbox, 1)
	require.IsType(t, &SendIncomingAckRequest{}, ackOutbox[0])

	fut = sess.FSM.AskEvent(ctx, &IncomingAckSentEvent{})
	result = fut.Await(ctx)
	require.False(t, result.IsErr())

	finalState, err := sess.FSM.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &ReceiveCompleted{}, finalState)
}

// TestReceiveSessionRetriesMetadataAfterRetryableMaterializationFailure
// verifies that receive sessions keep their state and re-query metadata when
// incoming materialization returns a retryable outbox failure.
func TestReceiveSessionRetriesMetadataAfterRetryableMaterializationFailure(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	arkPSBT, _, _, _, _, _ := buildTestIncomingMaterialization(t)
	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())

	sess, _, err := DriveIncomingTransfer(ctx, sessionID, arkPSBT)
	require.NoError(t, err)

	fut := sess.FSM.AskEvent(ctx, &IncomingMetadataResolvedEvent{})
	result := fut.Await(ctx)
	require.False(t, result.IsErr())

	outbox := result.UnwrapOr(nil)
	require.Len(t, outbox, 1)
	require.IsType(t, &MaterializeIncomingVTXOsRequest{}, outbox[0])

	fut = sess.FSM.AskEvent(ctx, &OutboxErrorEvent{
		OutboxType: (&MaterializeIncomingVTXOsRequest{}).outboxType(),
		Retryable:  true,
		RetryAfter: defaultRetryDelay,
		ErrorReason: "incoming metadata missing for " +
			"wallet-owned output 0",
	})
	result = fut.Await(ctx)
	require.False(t, result.IsErr())

	outbox = result.UnwrapOr(nil)
	require.Len(t, outbox, 1)
	schedule, ok := outbox[0].(*ScheduleRetryRequest)
	require.True(t, ok)
	require.Equal(t, defaultRetryDelay, schedule.After)

	fut = sess.FSM.AskEvent(ctx, &RetryDueEvent{})
	result = fut.Await(ctx)
	require.False(t, result.IsErr())

	// The give-up timer's RetryDueEvent re-queries metadata and re-arms the
	// timer keyed off the advanced attempt count.
	outbox = result.UnwrapOr(nil)
	require.Len(t, outbox, 2)
	require.IsType(t, &QueryIncomingMetadataRequest{}, outbox[0])
	require.IsType(t, &ScheduleRetryRequest{}, outbox[1])
}
