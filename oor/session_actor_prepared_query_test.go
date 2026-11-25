package oor

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/stretchr/testify/require"
)

// TestPreparedAbortQueryReloadsCommittedState prevents an uncommitted abort
// from authorizing channel cleanup before the durable OOR gate is closed.
func TestPreparedAbortQueryReloadsCommittedState(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	client, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	input := newTestTransferInput(
		t, client, operator.PubKey(), wire.OutPoint{
			Hash: [32]byte{0x83},
		},
		btcutil.Amount(10_000),
	)
	session, _, err := NewPreparedSessionWithIdempotencyKey(
		ctx, arkscript.CheckpointPolicy{
			OperatorKey: operator.PubKey(), CSVDelay: 10,
		}, []TransferInput{input}, []oortx.RecipientOutput{{
			PkScript: newTestTaprootPkScript(t, client.PubKey()),
			Value:    input.VTXO.Amount,
		}}, "", EnvConfig{},
	)
	require.NoError(t, err)
	initial, err := session.FSM.CurrentState()
	require.NoError(t, err)
	prepared, ok := initial.(*Prepared)
	require.True(t, ok)
	record, err := outgoingRegistryRecord(session.ID, prepared)
	require.NoError(t, err)
	base := newFakeRegistryStore()
	require.NoError(t, base.UpsertSession(ctx, record))
	store := &failFirstUpsertStore{
		fakeRegistryStore: base, failsLeft: 1,
	}
	releases := 0
	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore: store, ServerConn: fakeServerConnRef{},
			SpendReleaser: func(context.Context, []wire.OutPoint,
				map[wire.OutPoint]uint64) error {

				releases++

				return nil
			},
		},
		actorID: ActorIDForSession(session.ID), log: btclog.Disabled,
		sessionID: session.ID, direction: clientdb.OORSessionDirectionOutgoing,
		fsm: session.FSM, loaded: true,
	}
	t.Cleanup(b.stopFSM)
	ax := fakeExec{tx: oorTx{
		store: &fakeDeliveryStore{}, actorID: b.actorID,
	}}
	result := b.Receive(ctx, &DriveEventRequest{
		SessionID: session.ID,
		Event:     &AbortPreparedEvent{Reason: "negotiation canceled"},
	}, ax)
	require.True(t, result.IsErr())
	require.ErrorIs(t, result.Err(), errFilterBroken)
	require.Equal(t, 1, releases)
	require.True(t, b.commitFailed)
	uncommitted, err := b.fsm.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &Failed{}, uncommitted)
	saved, err := base.GetSession(ctx, chainHashOf(session.ID))
	require.NoError(t, err)
	require.Equal(t, record.Phase, saved.Phase)
	require.False(t, saved.Status.IsTerminal())

	result = b.Receive(ctx, &GetStateRequest{SessionID: session.ID}, ax)
	require.True(t, result.IsOk(), result.Err())
	response, ok := result.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(
		t, &Prepared{}, response.State, "status must describe the "+
			"committed gate, not the rolled-back abort",
	)
	require.False(t, b.commitFailed)
	require.Equal(t, 1, releases, "a read must not replay side effects")
	saved, err = base.GetSession(ctx, chainHashOf(session.ID))
	require.NoError(t, err)
	require.Equal(t, record.Phase, saved.Phase)
}
