package round

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/stretchr/testify/require"
)

// TestLocalOutboxPrecedesQueuedEffect keeps confirmation routing in the commit.
func TestLocalOutboxPrecedesQueuedEffect(t *testing.T) {
	id := RoundID{1}
	key := RoundKeyStr(id.KeyString())
	packet := &psbt.Packet{UnsignedTx: wire.NewMsgTx(2)}
	txid := packet.UnsignedTx.TxHash()
	machine := protofsm.NewInlineStateMachine(
		ClientStateMachineCfg{
			Logger: btclog.Disabled,
			InitialState: &InputSigSentState{
				RoundID:      id,
				CommitmentTx: packet,
			},
			Env: &ClientEnvironment{},
		},
	)
	machine.Start(t.Context())
	defer machine.Stop()
	round := &RoundFSM{FSM: &machine, Key: id, RoundID: id}
	actor := &RoundClientActor{
		log: btclog.Disabled,
		rounds: map[RoundKeyStr]*RoundFSM{
			key: round,
		},
		commitmentTxIndex: make(map[chainhash.Hash]RoundKeyStr),
	}
	var captured []ClientOutMsg
	actor.queueEffect = func(_ context.Context, msg ClientOutMsg) error {
		require.Equal(t, key, actor.commitmentTxIndex[txid])
		require.Equal(t, txid, round.TxID)
		require.Same(t, packet, round.CommitmentTx.UnwrapOr(nil))
		captured = append(captured, msg)

		return nil
	}
	confirmation := &RegisterConfirmationRequest{Txid: &txid}
	require.NoError(
		t,
		actor.processOutbox(
			t.Context(), []ClientOutMsg{
				&RoundCheckpointedNotification{RoundID: id},
				confirmation,
			},
		),
	)
	require.Equal(t, []ClientOutMsg{confirmation}, captured)
}

// TestLocalOutboxQueueFailure stops before recording any later effect.
func TestLocalOutboxQueueFailure(t *testing.T) {
	failed := errors.New("cannot encode effect")
	calls := 0
	actor := &RoundClientActor{
		queueEffect: func(context.Context, ClientOutMsg) error {
			calls++

			return failed
		},
	}
	require.ErrorIs(
		t,
		actor.processOutbox(
			t.Context(), []ClientOutMsg{
				&RegisterConfirmationRequest{},
				&StartTimeoutReq{},
			},
		),
		failed,
	)
	require.Equal(t, 1, calls)
}
