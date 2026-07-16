package virtualchannel

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

type fakeCoopCloseStore struct {
	changed bool
	err     error

	id            ID
	closeTx       *wire.MsgTx
	localBalance  btcutil.Amount
	remoteBalance btcutil.Amount
}

func (f *fakeCoopCloseStore) MarkVirtualChannelCoopClosed(_ context.Context,
	id ID, closeTx *wire.MsgTx,
	localBalance, remoteBalance btcutil.Amount) (bool, error) {

	f.id = id
	f.closeTx = closeTx
	f.localBalance = localBalance
	f.remoteBalance = remoteBalance

	return f.changed, f.err
}

type fakeCloseBalanceResolver struct {
	local   btcutil.Amount
	remote  btcutil.Amount
	handled bool
	err     error
}

func (f *fakeCloseBalanceResolver) ResolveCooperativeClose(_ context.Context,
	_ *Channel, _ *wire.MsgTx) (btcutil.Amount, btcutil.Amount, bool,
	error) {

	return f.local, f.remote, f.handled, f.err
}

// TestDurableCooperativeCloseSettlerPersistsHandledClose verifies that a
// resolved virtual close is durably recorded before lnd publish is suppressed.
func TestDurableCooperativeCloseSettlerPersistsHandledClose(t *testing.T) {
	t.Parallel()

	store := &fakeCoopCloseStore{
		changed: true,
	}
	resolver := &fakeCloseBalanceResolver{
		local:   btcutil.Amount(6000),
		remote:  btcutil.Amount(1000),
		handled: true,
	}
	settler, err := NewDurableCooperativeCloseSettler(
		DurableCooperativeCloseSettlerConfig{
			Store:           store,
			BalanceResolver: resolver,
		},
	)
	require.NoError(t, err)

	channel := &Channel{
		Registration: Registration{
			ID: fixedID(7),
		},
	}
	closeTx := wire.NewMsgTx(2)
	handled, err := settler.SettleCooperativeClose(
		t.Context(), channel, closeTx, "close",
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, channel.ID, store.id)
	require.Equal(t, closeTx, store.closeTx)
	require.Equal(t, resolver.local, store.localBalance)
	require.Equal(t, resolver.remote, store.remoteBalance)
}

// TestDurableCooperativeCloseSettlerDeclinesUnhandledClose verifies that
// ordinary lnd publishes continue when the resolver does not claim the close.
func TestDurableCooperativeCloseSettlerDeclinesUnhandledClose(t *testing.T) {
	t.Parallel()

	store := &fakeCoopCloseStore{
		changed: true,
	}
	resolver := &fakeCloseBalanceResolver{}
	settler, err := NewDurableCooperativeCloseSettler(
		DurableCooperativeCloseSettlerConfig{
			Store:           store,
			BalanceResolver: resolver,
		},
	)
	require.NoError(t, err)

	handled, err := settler.SettleCooperativeClose(
		t.Context(), &Channel{}, wire.NewMsgTx(2),
		"close",
	)
	require.NoError(t, err)
	require.False(t, handled)
	require.Nil(t, store.closeTx)
}

// TestDurableCooperativeCloseSettlerRequiresDurablePersistence verifies that a
// handled close never suppresses lnd publication unless the store transition
// succeeds.
func TestDurableCooperativeCloseSettlerRequiresDurablePersistence(
	t *testing.T) {

	t.Parallel()

	store := &fakeCoopCloseStore{}
	resolver := &fakeCloseBalanceResolver{
		handled: true,
	}
	settler, err := NewDurableCooperativeCloseSettler(
		DurableCooperativeCloseSettlerConfig{
			Store:           store,
			BalanceResolver: resolver,
		},
	)
	require.NoError(t, err)

	handled, err := settler.SettleCooperativeClose(
		t.Context(), &Channel{}, wire.NewMsgTx(2),
		"close",
	)
	require.ErrorContains(t, err, "was not persisted")
	require.False(t, handled)

	store.err = fmt.Errorf("db unavailable")
	handled, err = settler.SettleCooperativeClose(
		t.Context(), &Channel{}, wire.NewMsgTx(2),
		"close",
	)
	require.ErrorContains(t, err, "db unavailable")
	require.False(t, handled)
}
